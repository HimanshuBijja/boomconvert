package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"boomconvert/adapters"
)

type Converter struct {
	registry  *adapters.Registry
	store     *Store
	hub       *Hub
	backupDir string
}

func NewConverter(reg *adapters.Registry, store *Store, hub *Hub, backupDir string) *Converter {
	return &Converter{registry: reg, store: store, hub: hub, backupDir: backupDir}
}

// Run handles a single rename request: convert the file at originalPath (which
// holds the source content) to targetPath (the path with the new extension).
// Both paths are absolute. The caller is responsible for ignoring filesystem
// events on these paths while we work.
func (c *Converter) Run(ctx context.Context, originalPath, targetPath, srcFormat, dstFormat string) {
	rec := &ConversionRecord{
		SourcePath:   originalPath,
		TargetPath:   targetPath,
		SourceFormat: srcFormat,
		TargetFormat: dstFormat,
		StartedAt:    time.Now().UTC(),
		Status:       "running",
	}
	if info, err := os.Stat(originalPath); err == nil {
		rec.SourceSize = info.Size()
	}

	c.hub.Broadcast(Event{Type: "conversion_started", Payload: map[string]string{
		"source": originalPath, "target": targetPath,
		"source_format": srcFormat, "target_format": dstFormat,
	}})

	candidates := c.registry.LookupAll(srcFormat, dstFormat)
	if len(candidates) == 0 {
		c.failAndRevert(rec, originalPath, targetPath, fmt.Sprintf("no adapter for %s->%s (tool may be missing)", srcFormat, dstFormat))
		return
	}

	backupPath, err := c.backup(originalPath)
	if err != nil {
		c.failAndRevert(rec, originalPath, targetPath, "backup failed: "+err.Error())
		return
	}
	rec.BackupPath = backupPath

	tmpPath := tempSibling(targetPath)
	_ = os.Remove(tmpPath)

	// Try adapters in priority order; fall back to the next on failure. This
	// lets a flaky fast-path (e.g. unoserver) gracefully cede to a slower
	// reliable path (direct soffice) without surfacing a user-visible error.
	var convErr error
	var usedAdapter adapters.Adapter
	for _, adapter := range candidates {
		_ = os.Remove(tmpPath)
		cctx, cancel := context.WithTimeout(ctx, conversionTimeoutFor(srcFormat, dstFormat))
		err := adapter.Convert(cctx, originalPath, tmpPath, adapters.ConvertOptions{Quality: 90})
		cancel()
		if err == nil {
			if _, statErr := os.Stat(tmpPath); statErr == nil {
				convErr = nil
				usedAdapter = adapter
				break
			}
			err = fmt.Errorf("%s produced no output file", adapter.Name())
		}
		convErr = fmt.Errorf("%s: %w", adapter.Name(), err)
		c.hub.Broadcast(Event{Type: "adapter_fallback", Payload: map[string]string{
			"adapter": adapter.Name(),
			"error":   err.Error(),
		}})
	}
	_ = usedAdapter

	if convErr != nil {
		_ = os.Remove(tmpPath)
		c.restoreFromBackup(backupPath, originalPath, targetPath)
		c.failAndRevert(rec, originalPath, targetPath, convErr.Error())
		return
	}

	// Atomic finalize.
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmpPath)
		c.restoreFromBackup(backupPath, originalPath, targetPath)
		c.failAndRevert(rec, originalPath, targetPath, "could not clear target: "+err.Error())
		return
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		c.restoreFromBackup(backupPath, originalPath, targetPath)
		c.failAndRevert(rec, originalPath, targetPath, "finalize rename failed: "+err.Error())
		return
	}

	// Originals are kept in the backup folder for restoration; the active
	// folder only holds the converted file.
	if originalPath != targetPath {
		_ = os.Remove(originalPath)
	}

	if info, err := os.Stat(targetPath); err == nil {
		rec.TargetSize = info.Size()
	}
	rec.Status = "completed"
	rec.FinishedAt = time.Now().UTC()
	_ = c.store.InsertConversion(rec)

	c.hub.Broadcast(Event{Type: "conversion_completed", Payload: rec})
}

func (c *Converter) failAndRevert(rec *ConversionRecord, originalPath, targetPath, msg string) {
	rec.Status = "failed"
	rec.Error = msg
	rec.FinishedAt = time.Now().UTC()
	_ = c.store.InsertConversion(rec)
	c.hub.Broadcast(Event{Type: "conversion_failed", Payload: rec})
}

func (c *Converter) backup(src string) (string, error) {
	if err := os.MkdirAll(c.backupDir, 0o755); err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102-150405.000")
	dst := filepath.Join(c.backupDir, stamp+"_"+filepath.Base(src))
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return dst, nil
}

func (c *Converter) restoreFromBackup(backupPath, originalPath, targetPath string) {
	if backupPath == "" {
		return
	}
	// Restore original content under the *original* extension so the user
	// sees their file back the way it was.
	if originalPath != targetPath {
		_ = os.Remove(targetPath)
	}
	in, err := os.Open(backupPath)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(originalPath)
	if err != nil {
		return
	}
	_, _ = io.Copy(out, in)
	_ = out.Close()
}

// RestoreBackup copies backupPath back to originalPath and (optionally) removes
// the converted target file. Returns the size of the restored file.
func (c *Converter) RestoreBackup(backupPath, originalPath, convertedPath string, deleteConverted bool) (int64, error) {
	if _, err := os.Stat(backupPath); err != nil {
		return 0, fmt.Errorf("backup not found: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(originalPath), 0o755); err != nil {
		return 0, err
	}
	in, err := os.Open(backupPath)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp := tempSibling(originalPath)
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, originalPath); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if deleteConverted && convertedPath != "" && convertedPath != originalPath {
		_ = os.Remove(convertedPath)
	}
	return n, nil
}

func conversionTimeoutFor(src, dst string) time.Duration {
	if isImage(src) && isImage(dst) {
		return 60 * time.Second
	}
	return 300 * time.Second
}

// tempSibling returns a tmp path that preserves the original extension, so
// extension-aware tools (ImageMagick, LibreOffice) infer the output format
// correctly. e.g. /a/b/foo.png -> /a/b/foo.bc-tmp.png
func tempSibling(target string) string {
	dir, base := filepath.Split(target)
	ext := filepath.Ext(base)
	stem := base[:len(base)-len(ext)]
	return filepath.Join(dir, stem+".bc-tmp"+ext)
}

func isImage(f string) bool {
	switch strings.ToLower(f) {
	case "jpg", "jpeg", "png", "gif", "webp", "bmp", "tiff", "tif", "ico":
		return true
	}
	return false
}

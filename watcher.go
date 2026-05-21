package main

import (
	"archive/zip"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/h2non/filetype"
)

type Watcher struct {
	fsw       *fsnotify.Watcher
	converter *Converter
	hub       *Hub

	mu          sync.Mutex
	ignored     map[string]time.Time // path -> expiry
	pendingRem  map[string]pendingRemove

	paused bool
}

type pendingRemove struct {
	path string
	size int64
	at   time.Time
}

func NewWatcher(c *Converter, hub *Hub) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		fsw:        fsw,
		converter:  c,
		hub:        hub,
		ignored:    map[string]time.Time{},
		pendingRem: map[string]pendingRemove{},
	}, nil
}

func (w *Watcher) SetPaused(p bool) {
	w.mu.Lock()
	w.paused = p
	w.mu.Unlock()
}

func (w *Watcher) IsPaused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused
}

func (w *Watcher) AddRoot(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return w.fsw.Add(p)
		}
		return nil
	})
}

func (w *Watcher) RemoveRoot(root string) error {
	// fsnotify lacks recursive remove; walk known watches via WalkDir.
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = w.fsw.Remove(p)
		}
		return nil
	})
}

func (w *Watcher) ignoreFor(path string, d time.Duration) {
	w.mu.Lock()
	w.ignored[path] = time.Now().Add(d)
	w.mu.Unlock()
}

func (w *Watcher) isIgnored(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	exp, ok := w.ignored[path]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(w.ignored, path)
		return false
	}
	return true
}

func (w *Watcher) Run(ctx context.Context) {
	gc := time.NewTicker(30 * time.Second)
	defer gc.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gc.C:
			w.gc()
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			log.Printf("fsnotify error: %v", err)
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ctx, ev)
		}
	}
}

func (w *Watcher) gc() {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, v := range w.ignored {
		if now.After(v) {
			delete(w.ignored, k)
		}
	}
	for k, v := range w.pendingRem {
		if now.Sub(v.at) > 2*time.Second {
			delete(w.pendingRem, k)
		}
	}
}

func (w *Watcher) handle(ctx context.Context, ev fsnotify.Event) {
	if w.IsPaused() {
		return
	}
	if w.isIgnored(ev.Name) {
		return
	}

	// If a new directory was created inside a watched root, watch it too.
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			_ = w.fsw.Add(ev.Name)
			return
		}
	}

	// Skip our own backup dir if it lives inside the watched tree (safety).
	if strings.Contains(strings.ToLower(filepath.ToSlash(ev.Name)), "/.backup/") {
		return
	}

	// Skip work artifacts.
	if strings.Contains(filepath.Base(ev.Name), ".bc-tmp.") {
		return
	}

	switch {
	case ev.Op&fsnotify.Rename != 0:
		// On Linux this fires for the OLD name (now gone); the NEW name will arrive as CREATE.
		// We register the pending remove and try to match it in CREATE.
		w.recordPendingRemove(ev.Name)
	case ev.Op&fsnotify.Remove != 0:
		w.recordPendingRemove(ev.Name)
	case ev.Op&fsnotify.Create != 0:
		w.handleCreate(ctx, ev.Name)
	}
}

func (w *Watcher) recordPendingRemove(path string) {
	w.mu.Lock()
	w.pendingRem[strings.ToLower(filepath.Base(path))] = pendingRemove{
		path: path,
		at:   time.Now(),
	}
	w.mu.Unlock()
}

// handleCreate is invoked when a new path appears. If it matches a recently-removed
// path with only its extension changed, we treat that as a "rename to convert" gesture.
func (w *Watcher) handleCreate(ctx context.Context, newPath string) {
	info, err := os.Stat(newPath)
	if err != nil || info.IsDir() {
		return
	}

	newExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(newPath)), ".")
	if newExt == "" {
		return
	}
	newStem := strings.TrimSuffix(filepath.Base(newPath), filepath.Ext(newPath))

	// Look for any pending-remove whose stem matches and whose extension differs.
	w.mu.Lock()
	var matched pendingRemove
	var matchedKey string
	for k, pr := range w.pendingRem {
		prBase := filepath.Base(pr.path)
		prStem := strings.TrimSuffix(prBase, filepath.Ext(prBase))
		prExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(prBase)), ".")
		if strings.EqualFold(prStem, newStem) && prExt != newExt && prExt != "" {
			matched = pr
			matchedKey = k
			break
		}
	}
	if matchedKey != "" {
		delete(w.pendingRem, matchedKey)
	}
	w.mu.Unlock()

	if matchedKey == "" {
		return
	}

	// Determine the true source format. We prefer magic-byte detection but
	// fall back to the original (pre-rename) extension when bytes are
	// inconclusive or generic (e.g. all OOXML formats look like "zip").
	originalExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(matched.path)), ".")
	srcFormat := detectFormatWithFallback(newPath, originalExt)
	if srcFormat == "" {
		log.Printf("watcher: cannot detect source format of %s", newPath)
		return
	}
	if normalizeExt(srcFormat) == normalizeExt(newExt) {
		// Signature already matches extension; treat as manual correction.
		return
	}

	// Reserve paths to ignore feedback events from our own write.
	w.ignoreFor(matched.path, 5*time.Second)
	w.ignoreFor(newPath, 30*time.Second)

	// Move the user-renamed file back to its original name (we'll convert FROM there),
	// because the renamed file still holds the OLD bytes.
	if matched.path != newPath {
		_ = os.Rename(newPath, matched.path)
	}

	go w.converter.Run(ctx, matched.path, newPath, srcFormat, newExt)
}

func detectFormatWithFallback(path, originalExt string) string {
	f, err := os.Open(path)
	if err != nil {
		return originalExt
	}
	head := make([]byte, 261)
	n, _ := f.Read(head)
	f.Close()
	kind, _ := filetype.Match(head[:n])

	if kind != filetype.Unknown && kind.Extension != "zip" {
		return kind.Extension
	}
	if kind.Extension == "zip" {
		if ooxml := sniffOOXML(path); ooxml != "" {
			return ooxml
		}
	}
	return originalExt
}

func sniffOOXML(path string) string {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return ""
	}
	defer zr.Close()
	for _, f := range zr.File {
		switch {
		case strings.HasPrefix(f.Name, "word/"):
			return "docx"
		case strings.HasPrefix(f.Name, "ppt/"):
			return "pptx"
		case strings.HasPrefix(f.Name, "xl/"):
			return "xlsx"
		}
	}
	return ""
}

func normalizeExt(e string) string {
	e = strings.ToLower(strings.TrimPrefix(e, "."))
	switch e {
	case "jpeg":
		return "jpg"
	case "tif":
		return "tiff"
	}
	return e
}

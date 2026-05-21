package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RetentionPolicy controls how aggressively old backups are pruned.
// MaxAge: files older than this are deleted regardless of size budget.
// MaxBytes: after age pruning, the oldest files are deleted until total
// directory size is within this cap.
// Zero values disable that dimension.
type RetentionPolicy struct {
	MaxAge   time.Duration `json:"max_age_ns"`
	MaxBytes int64         `json:"max_bytes"`
}

func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		MaxAge:   30 * 24 * time.Hour,
		MaxBytes: 2 * 1024 * 1024 * 1024, // 2 GB
	}
}

// LoadRetentionPolicy reads the policy from the settings table, falling back
// to the default if not set or unreadable.
func LoadRetentionPolicy(store *Store) RetentionPolicy {
	p := DefaultRetentionPolicy()
	if store == nil {
		return p
	}
	_, _ = store.GetSetting("backup_retention", &p)
	return p
}

// SweepBackups deletes backups according to policy and clears the
// backup_path column for any conversion whose backup file is now gone.
// Returns the count of files deleted and total bytes freed.
func SweepBackups(store *Store, backupDir string, policy RetentionPolicy) (int, int64, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	type fileEntry struct {
		path  string
		size  int64
		mtime time.Time
	}
	files := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileEntry{
			path:  filepath.Join(backupDir, e.Name()),
			size:  info.Size(),
			mtime: info.ModTime(),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })

	now := time.Now()
	var deleted int
	var freed int64
	remaining := files[:0]

	// Pass 1: age-based deletion.
	for _, f := range files {
		if policy.MaxAge > 0 && now.Sub(f.mtime) > policy.MaxAge {
			if err := os.Remove(f.path); err == nil {
				deleted++
				freed += f.size
				clearBackupRef(store, f.path)
			}
			continue
		}
		remaining = append(remaining, f)
	}

	// Pass 2: size-cap deletion, oldest first.
	if policy.MaxBytes > 0 {
		var total int64
		for _, f := range remaining {
			total += f.size
		}
		i := 0
		for total > policy.MaxBytes && i < len(remaining) {
			f := remaining[i]
			if err := os.Remove(f.path); err == nil {
				deleted++
				freed += f.size
				total -= f.size
				clearBackupRef(store, f.path)
			}
			i++
		}
	}

	return deleted, freed, nil
}

func clearBackupRef(store *Store, backupPath string) {
	if store == nil {
		return
	}
	_, _ = store.db.Exec(`UPDATE conversions SET backup_path='' WHERE backup_path=?`, backupPath)
}

// RunRetentionLoop sweeps once at startup, then on a fixed cadence until ctx is done.
func RunRetentionLoop(ctx context.Context, store *Store, backupDir string, every time.Duration) {
	sweep := func() {
		policy := LoadRetentionPolicy(store)
		n, freed, err := SweepBackups(store, backupDir, policy)
		if err != nil {
			log.Printf("retention sweep error: %v", err)
			return
		}
		if n > 0 {
			log.Printf("retention: pruned %d backup file(s), freed %d bytes", n, freed)
		}
	}
	sweep()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

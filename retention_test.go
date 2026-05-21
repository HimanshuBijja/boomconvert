package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path string, size int, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestSweepBackups_AgeBased(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mustWrite(t, filepath.Join(dir, "old.bin"), 100, now.Add(-40*24*time.Hour))
	mustWrite(t, filepath.Join(dir, "fresh.bin"), 100, now.Add(-1*time.Hour))

	n, freed, err := SweepBackups(nil, dir, RetentionPolicy{MaxAge: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || freed != 100 {
		t.Fatalf("want 1 deleted/100 freed, got %d/%d", n, freed)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.bin")); !os.IsNotExist(err) {
		t.Fatal("old.bin should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.bin")); err != nil {
		t.Fatal("fresh.bin should remain")
	}
}

func TestSweepBackups_SizeCap(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// Three files, each 1 KiB, varying mtimes. Cap at 2 KiB -> oldest pruned.
	mustWrite(t, filepath.Join(dir, "a.bin"), 1024, now.Add(-3*time.Hour))
	mustWrite(t, filepath.Join(dir, "b.bin"), 1024, now.Add(-2*time.Hour))
	mustWrite(t, filepath.Join(dir, "c.bin"), 1024, now.Add(-1*time.Hour))

	n, freed, err := SweepBackups(nil, dir, RetentionPolicy{MaxBytes: 2 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || freed != 1024 {
		t.Fatalf("want 1 deleted/1024 freed, got %d/%d", n, freed)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.bin")); !os.IsNotExist(err) {
		t.Fatal("oldest should be pruned first")
	}
}

func TestSweepBackups_MissingDir(t *testing.T) {
	n, freed, err := SweepBackups(nil, filepath.Join(t.TempDir(), "does-not-exist"), DefaultRetentionPolicy())
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if n != 0 || freed != 0 {
		t.Fatalf("expected no-op on missing dir")
	}
}

func TestSweepBackups_ZeroPolicyPreservesAll(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.bin"), 100, time.Now().Add(-365*24*time.Hour))

	n, _, err := SweepBackups(nil, dir, RetentionPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("zero policy should preserve everything, deleted %d", n)
	}
}

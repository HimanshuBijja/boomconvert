package main

import (
	"os"
	"path/filepath"
	"runtime"
)

type AppPaths struct {
	Root       string
	DBPath     string
	BackupDir  string
	DefaultWatch string
}

func ResolveAppPaths() (AppPaths, error) {
	var root string
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			root = filepath.Join(appdata, "BoomConvert")
		}
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return AppPaths{}, err
		}
		root = filepath.Join(home, ".boomconvert")
	}

	p := AppPaths{
		Root:      root,
		DBPath:    filepath.Join(root, "boomconvert.db"),
		BackupDir: filepath.Join(root, "backups"),
	}

	home, _ := os.UserHomeDir()
	p.DefaultWatch = filepath.Join(home, "BoomConvert-Magic")

	for _, d := range []string{p.Root, p.BackupDir, p.DefaultWatch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return AppPaths{}, err
		}
	}
	return p, nil
}

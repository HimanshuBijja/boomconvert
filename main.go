package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"boomconvert/adapters"
)

//go:embed frontend/*
var embeddedFrontend embed.FS

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	paths, err := ResolveAppPaths()
	if err != nil {
		log.Fatalf("paths: %v", err)
	}
	log.Printf("config root: %s", paths.Root)

	store, err := OpenStore(paths.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	tools := NewToolHealth()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tools.Probe(ctx)
	for _, t := range tools.Snapshot() {
		if t.Found {
			log.Printf("tool %-14s OK  %s", t.ID, t.Version)
		} else {
			log.Printf("tool %-14s MISSING  install: %s", t.ID, t.InstallHint)
		}
	}

	registry := adapters.NewRegistry(tools)
	registry.Register(adapters.ImageMagickAdapter{})

	// UnoAdapter must come before LibreOfficeAdapter so it wins for the
	// rules they both cover when unoserver is installed.
	unoMgr := adapters.NewUnoManager()
	// On Windows, unoserver must be invoked via LibreOffice's bundled Python
	// (the pip-installed wrapper uses the wrong interpreter and exits with
	// "no uno module"). Derive python.exe from the detected soffice.exe path.
	if soffice := tools.PathFor("libreoffice"); soffice != "" {
		loPy := filepath.Join(filepath.Dir(soffice), "python.exe")
		if _, err := os.Stat(loPy); err == nil {
			unoMgr.LOPython = loPy
		}
	}
	if uno := tools.PathFor("unoserver"); uno != "" {
		// unoconvert lives next to unoserver.exe in the same Scripts dir.
		unoMgr.UnoconvertExe = filepath.Join(filepath.Dir(uno), "unoconvert.exe")
	}
	defer unoMgr.Stop()
	registry.Register(adapters.UnoAdapter{Manager: unoMgr})
	registry.Register(adapters.LibreOfficeAdapter{})
	registry.Register(adapters.Pdf2DocxAdapter{})

	hub := NewHub()
	converter := NewConverter(registry, store, hub, paths.BackupDir)

	watcher, err := NewWatcher(converter, hub)
	if err != nil {
		log.Fatalf("watcher: %v", err)
	}

	// Ensure at least one watched folder exists.
	existing, _ := store.ListFolders()
	if len(existing) == 0 {
		if _, err := store.AddFolder(paths.DefaultWatch); err != nil {
			log.Printf("warn: could not seed default folder: %v", err)
		}
		log.Printf("seeded default watch folder: %s", paths.DefaultWatch)
	}

	var foldersMu sync.Mutex
	syncWatches := func() {
		foldersMu.Lock()
		defer foldersMu.Unlock()
		folders, err := store.ListFolders()
		if err != nil {
			log.Printf("listFolders: %v", err)
			return
		}
		for _, f := range folders {
			if f.Enabled {
				if err := watcher.AddRoot(f.Path); err != nil {
					log.Printf("watch %s: %v", f.Path, err)
				}
			}
		}
	}
	syncWatches()

	go watcher.Run(ctx)
	go RunRetentionLoop(ctx, store, paths.BackupDir, 1*time.Hour)

	staticFS, err := fs.Sub(embeddedFrontend, "frontend")
	if err != nil {
		log.Fatalf("frontend embed: %v", err)
	}

	srv := NewServer(store, tools, registry, watcher, hub, converter, paths.BackupDir, staticFS, syncWatches)

	httpSrv := &http.Server{
		Addr:    "127.0.0.1:8001",
		Handler: srv.Routes(),
	}

	go func() {
		log.Printf("dashboard: http://127.0.0.1:8001")
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down...")
	shutdownCtx, cancelSh := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSh()
	_ = httpSrv.Shutdown(shutdownCtx)
}

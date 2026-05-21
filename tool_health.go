package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type ToolStatus struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Found       bool   `json:"found"`
	Path        string `json:"path"`
	Version     string `json:"version"`
	InstallHint string `json:"install_hint"`
}

type ToolHealth struct {
	mu   sync.RWMutex
	byID map[string]ToolStatus
}

func NewToolHealth() *ToolHealth {
	return &ToolHealth{byID: map[string]ToolStatus{}}
}

func (t *ToolHealth) Available(id string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byID[id].Found
}

// PathFor returns the absolute executable path detected for the given tool id,
// or empty if the tool was not found.
func (t *ToolHealth) PathFor(id string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byID[id].Path
}

func (t *ToolHealth) Snapshot() []ToolStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]ToolStatus, 0, len(t.byID))
	for _, s := range t.byID {
		out = append(out, s)
	}
	return out
}

type probeSpec struct {
	id          string
	display     string
	exeNames    []string // candidate executable names to LookPath
	fallbackGlobs []string // Windows install-location globs to scan if LookPath fails
	probeArgs   []string // args passed to the resolved executable to gather a version
	versionLine func(out string, err error) (string, bool) // (version, ok)
	installHint string
}

func defaultVersionExtractor(out string, err error) (string, bool) {
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line, true
		}
	}
	return "", true
}

func pdf2docxVersionExtractor(out string, err error) (string, bool) {
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(out)
	if v == "" {
		return "", false
	}
	return "pdf2docx " + v, true
}

func (t *ToolHealth) Probe(ctx context.Context) {
	pf := programFilesRoots()

	specs := []probeSpec{
		{
			id: "imagemagick", display: "ImageMagick",
			exeNames: []string{"magick"},
			fallbackGlobs: globsUnder(pf, "ImageMagick*", "magick.exe"),
			probeArgs:    []string{"-version"},
			versionLine:  defaultVersionExtractor,
			installHint:  "winget install ImageMagick.ImageMagick",
		},
		{
			id: "libreoffice", display: "LibreOffice",
			exeNames: []string{"soffice"},
			fallbackGlobs: globsUnder(pf, "LibreOffice/program", "soffice.exe"),
			probeArgs:    []string{"--version"},
			versionLine:  defaultVersionExtractor,
			installHint:  "winget install TheDocumentFoundation.LibreOffice",
		},
		{
			id: "pdf2docx", display: "pdf2docx (Python)",
			exeNames: []string{"python", "python3", "py"},
			// pdf2docx is a Python module; probe via importlib.metadata.
			probeArgs:   []string{"-c", "import importlib.metadata,sys; sys.stdout.write(importlib.metadata.version('pdf2docx'))"},
			versionLine: pdf2docxVersionExtractor,
			installHint: "pip install pdf2docx",
		},
		{
			id: "unoserver", display: "unoserver (fast LibreOffice)",
			exeNames:      []string{"unoserver"},
			fallbackGlobs: unoserverFallbackGlobs(pf),
			probeArgs:     []string{"--help"},
			versionLine:   defaultVersionExtractor,
			installHint:   `& "C:\Program Files\LibreOffice\program\python.exe" -m pip install unoserver`,
		},
		{
			id: "poppler", display: "Poppler (pdftoppm)",
			exeNames: []string{"pdftoppm"},
			fallbackGlobs: globsUnder(pf, "poppler*/Library/bin", "pdftoppm.exe"),
			probeArgs:    []string{"-v"},
			versionLine:  defaultVersionExtractor,
			installHint:  "winget install oschwartz10612.Poppler",
		},
	}

	results := make(map[string]ToolStatus, len(specs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range specs {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			status := ToolStatus{ID: s.id, DisplayName: s.display, InstallHint: s.installHint}
			exePath := resolveExe(s.exeNames, s.fallbackGlobs)
			if exePath != "" {
				status.Path = exePath
				// Filesystem detection alone is enough to mark Found for tools
				// detected via fallback globs (we just stat'd the file). For
				// tools resolved through PATH we still trust LookPath. The
				// version probe is best-effort with a tight timeout — some
				// Windows GUI-subsystem binaries (notably soffice.exe) never
				// flush stdout to a console parent, so we cap it short.
				status.Found = true
				pctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
				out, err := exec.CommandContext(pctx, exePath, s.probeArgs...).CombinedOutput()
				cancel()
				if ver, ok := s.versionLine(string(out), err); ok {
					status.Version = ver
				}
			}
			mu.Lock()
			results[s.id] = status
			mu.Unlock()
		}()
	}
	wg.Wait()

	t.mu.Lock()
	t.byID = results
	t.mu.Unlock()

	t.prependDiscoveredDirsToPath()
}

// prependDiscoveredDirsToPath adds the parent directories of every found tool
// to this process's PATH. This makes exec.Command(name, ...) work for adapters
// even when the shell that started us has a stale PATH (common on Windows
// after a fresh install).
func (t *ToolHealth) prependDiscoveredDirsToPath() {
	t.mu.RLock()
	defer t.mu.RUnlock()

	sep := string(os.PathListSeparator)
	current := os.Getenv("PATH")
	seen := map[string]bool{}
	for _, p := range strings.Split(current, sep) {
		seen[strings.ToLower(p)] = true
	}
	var prepend []string
	for _, s := range t.byID {
		if !s.Found || s.Path == "" {
			continue
		}
		dir := filepath.Dir(s.Path)
		if dir == "" || seen[strings.ToLower(dir)] {
			continue
		}
		prepend = append(prepend, dir)
		seen[strings.ToLower(dir)] = true
	}
	if len(prepend) == 0 {
		return
	}
	os.Setenv("PATH", strings.Join(prepend, sep)+sep+current)
}

func resolveExe(exeNames []string, fallbackGlobs []string) string {
	for _, name := range exeNames {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	for _, g := range fallbackGlobs {
		matches, _ := filepath.Glob(g)
		for _, m := range matches {
			if info, err := os.Stat(m); err == nil && !info.IsDir() {
				return m
			}
		}
	}
	return ""
}

func programFilesRoots() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	roots := []string{}
	for _, e := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		if v := os.Getenv(e); v != "" {
			roots = append(roots, v)
		}
	}
	if len(roots) == 0 {
		roots = []string{`C:\Program Files`, `C:\Program Files (x86)`}
	}
	return roots
}

// unoserverFallbackGlobs covers the common Windows install layouts:
// - alongside LibreOffice's bundled Python
// - in the user's per-Python Scripts dir (where `pip install --user` puts it)
func unoserverFallbackGlobs(pf []string) []string {
	var out []string
	out = append(out, globsUnder(pf, "LibreOffice/program", "unoserver.exe")...)
	out = append(out, globsUnder(pf, "LibreOffice/program/python-core-*/Scripts", "unoserver.exe")...)
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		out = append(out, filepath.Join(appdata, "Python", "Python*", "Scripts", "unoserver.exe"))
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		out = append(out, filepath.Join(local, "Programs", "Python", "Python*", "Scripts", "unoserver.exe"))
	}
	return out
}

func globsUnder(roots []string, subdirPattern, exeName string) []string {
	if len(roots) == 0 {
		return nil
	}
	var out []string
	for _, r := range roots {
		out = append(out, filepath.Join(r, subdirPattern, exeName))
	}
	return out
}

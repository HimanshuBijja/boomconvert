package adapters

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// UnoManager owns a long-running `unoserver` process and proxies conversion
// requests to it via `unoconvert`. The first conversion pays the LibreOffice
// boot cost (~5-10s); subsequent conversions complete in ~1s.
//
// The manager idles itself after IdleTimeout of inactivity to free RAM
// (LibreOffice keeps ~150MB resident).
type UnoManager struct {
	IdleTimeout time.Duration

	// LOPython is the path to LibreOffice's bundled python.exe. Required on
	// Windows because the pip-installed `unoserver` wrapper uses the system
	// Python, which lacks UNO bindings. If empty, falls back to running the
	// `unoserver` command directly (works on Linux distros where LO's Python
	// is the system Python).
	LOPython string

	// UnoconvertExe is the path to `unoconvert` (also installed by pip). On
	// Windows this is the wrapper at AppData/.../Scripts/unoconvert.exe;
	// it works as a client even without UNO because it only talks to the
	// running server over a socket.
	UnoconvertExe string

	mu        sync.Mutex
	proc      *exec.Cmd
	procDone  chan struct{}
	ready     chan struct{}
	startErr  error
	idleTimer *time.Timer
	port      string
	host      string

	stoppedExplicitly bool

	// Simple circuit breaker. After N consecutive failures we mark this
	// manager unusable for the rest of the session so the converter stops
	// paying the Uno startup tax on every conversion.
	consecFailures int
	disabled       bool
}

const unoMaxConsecFailures = 2

func NewUnoManager() *UnoManager {
	return &UnoManager{
		IdleTimeout: 5 * time.Minute,
		host:        "127.0.0.1",
		port:        "2202",
	}
}

// Stop terminates the unoserver process (if running). Safe to call when stopped.
func (m *UnoManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stoppedExplicitly = true
	m.stopLocked()
}

func (m *UnoManager) stopLocked() {
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	if m.proc != nil && m.proc.Process != nil {
		_ = m.proc.Process.Kill()
	}
	m.proc = nil
	m.ready = nil
	m.procDone = nil
}

// Convert routes a conversion request through unoserver/unoconvert. Falls back
// to a clear error if the helper can't be started.
func (m *UnoManager) Convert(ctx context.Context, src, dst, dstFormat string) error {
	m.mu.Lock()
	if m.disabled {
		m.mu.Unlock()
		return errors.New("uno disabled after repeated failures")
	}
	m.mu.Unlock()

	// Cap individual Uno conversions tightly. Cold-start adds ~5-10s for the
	// first call; warm calls return in ~1-2s. A long wall-clock here almost
	// always means LibreOffice has crashed underneath unoconvert (a known
	// Windows/AV interaction), and we want the converter to fall through to
	// the direct-soffice adapter rather than waiting for the outer doc
	// timeout (which can be minutes).
	innerCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	err := m.convertInner(ctx, innerCtx, cancel, src, dst, dstFormat)
	m.mu.Lock()
	if err != nil {
		m.consecFailures++
		if m.consecFailures >= unoMaxConsecFailures {
			m.disabled = true
			log.Printf("uno: disabled for this session after %d consecutive failures", m.consecFailures)
			// Tear down any running server.
			m.stopLocked()
		}
	} else {
		m.consecFailures = 0
	}
	m.mu.Unlock()
	return err
}

func (m *UnoManager) convertInner(ctx, innerCtx context.Context, cancel context.CancelFunc, src, dst, dstFormat string) error {

	if err := m.ensureRunning(innerCtx); err != nil {
		return err
	}
	// Reset idle timer on each use.
	m.mu.Lock()
	if m.idleTimer != nil {
		m.idleTimer.Reset(m.IdleTimeout)
	}
	procDone := m.procDone
	m.mu.Unlock()
	_ = ctx // reserved for future extension points

	var cmd *exec.Cmd
	if m.LOPython != "" {
		script := fmt.Sprintf(
			`import sys; from unoserver.client import converter_main; sys.argv = ['unoconvert','--host',%q,'--port',%q,'--convert-to',%q,%q,%q]; sys.exit(converter_main() or 0)`,
			m.host, m.port, dstFormat, src, dst,
		)
		cmd = exec.CommandContext(innerCtx, m.LOPython, "-c", script)
	} else {
		args := []string{
			"--host", m.host,
			"--port", m.port,
			"--convert-to", dstFormat,
			src, dst,
		}
		exe := m.UnoconvertExe
		if exe == "" {
			exe = "unoconvert"
		}
		cmd = exec.CommandContext(innerCtx, exe, args...)
	}

	// Run in a goroutine so we can also wake on the unoserver process dying.
	type result struct {
		out []byte
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		resCh <- result{out: out, err: err}
	}()

	select {
	case r := <-resCh:
		if r.err != nil {
			return fmt.Errorf("unoconvert: %w: %s", r.err, string(r.out))
		}
		if _, statErr := os.Stat(dst); statErr != nil {
			return fmt.Errorf("unoconvert produced no output at %s: %s", dst, string(r.out))
		}
		return nil
	case <-procDone:
		// The server died underneath us. Cancel the in-flight unoconvert and
		// surface a clear error so the converter falls back.
		cancel()
		<-resCh
		return errors.New("unoserver died during conversion")
	case <-innerCtx.Done():
		<-resCh
		return innerCtx.Err()
	}
}

func (m *UnoManager) ensureRunning(ctx context.Context) error {
	m.mu.Lock()
	if m.proc != nil {
		ready := m.ready
		m.mu.Unlock()
		// Wait for the (possibly in-flight) startup to complete.
		select {
		case <-ready:
			m.mu.Lock()
			err := m.startErr
			m.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.stoppedExplicitly {
		m.mu.Unlock()
		return errors.New("uno manager stopped")
	}

	m.ready = make(chan struct{})
	m.procDone = make(chan struct{})
	m.startErr = nil
	procDone := m.procDone
	ready := m.ready

	var cmd *exec.Cmd
	if m.LOPython != "" {
		cmd = exec.Command(m.LOPython, "-m", "unoserver.server",
			"--interface", m.host,
			"--port", m.port,
		)
	} else {
		cmd = exec.Command("unoserver",
			"--interface", m.host,
			"--port", m.port,
		)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.startErr = err
		close(ready)
		m.proc = nil
		m.mu.Unlock()
		return err
	}
	stdout, _ := cmd.StdoutPipe()

	if err := cmd.Start(); err != nil {
		m.startErr = err
		close(ready)
		m.proc = nil
		m.mu.Unlock()
		return err
	}
	m.proc = cmd
	log.Printf("uno: starting unoserver (host=%s port=%s)", m.host, m.port)

	// Pipe scraper goroutine: closes `ready` once we see the listening banner
	// or after a fallback delay (some unoserver versions print to different
	// streams).
	go m.scrapeReady(stderr, stdout, ready)
	go func() {
		_ = cmd.Wait()
		close(procDone)
		m.mu.Lock()
		if m.proc == cmd {
			m.proc = nil
			m.ready = nil
		}
		m.mu.Unlock()
		log.Printf("uno: unoserver exited")
	}()

	// Schedule idle shutdown.
	if m.idleTimer == nil {
		m.idleTimer = time.AfterFunc(m.IdleTimeout, m.idleShutdown)
	} else {
		m.idleTimer.Reset(m.IdleTimeout)
	}
	m.mu.Unlock()

	select {
	case <-ready:
		m.mu.Lock()
		err := m.startErr
		m.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-procDone:
		return errors.New("unoserver exited before becoming ready")
	}
}

func (m *UnoManager) scrapeReady(stderr io.ReadCloser, stdout io.ReadCloser, ready chan struct{}) {
	bannerSeen := make(chan struct{}, 1)
	scan := func(r io.Reader, label string) {
		s := bufio.NewScanner(r)
		for s.Scan() {
			line := s.Text()
			log.Printf("uno[%s]: %s", label, line)
			// unoserver banner mentions "listening" once ready.
			if containsAny(line, "Listening", "listening", "Started", "started", "Server") {
				select {
				case bannerSeen <- struct{}{}:
				default:
				}
			}
		}
	}
	go scan(stderr, "err")
	go scan(stdout, "out")

	// Fall back to a fixed grace period if no recognizable banner appears.
	select {
	case <-bannerSeen:
	case <-time.After(20 * time.Second):
	}
	close(ready)
}

func (m *UnoManager) idleShutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proc == nil {
		return
	}
	log.Printf("uno: idle timeout reached, terminating unoserver")
	if m.proc.Process != nil {
		_ = m.proc.Process.Kill()
	}
}

// UnoAdapter is preferred over LibreOfficeAdapter when unoserver is installed.
// It supports the same DOCX/PPTX -> PDF rules.
type UnoAdapter struct {
	Manager *UnoManager
}

func (UnoAdapter) Name() string         { return "uno" }
func (UnoAdapter) RequiredTool() string { return "unoserver" }

func (UnoAdapter) Supports(src, dst string) bool {
	switch {
	case src == "docx" && dst == "pdf":
		return true
	case src == "pptx" && dst == "pdf":
		return true
	}
	return false
}

func (a UnoAdapter) Convert(ctx context.Context, src, dst string, _ ConvertOptions) error {
	if a.Manager == nil {
		return errors.New("uno manager not initialized")
	}
	dstExt := Canonical(extOf(dst))
	// unoconvert writes directly to dst; route through a temp file in the
	// same directory so the watcher's atomic-rename pattern still applies
	// upstream. Here we just hand `dst` straight through — converter.go
	// already invokes us with the tmp path it intends to atomically rename.
	return a.Manager.Convert(ctx, src, dst, dstExt)
}

func (UnoAdapter) Rules() []Rule {
	return []Rule{
		{Source: "docx", Target: "pdf"},
		{Source: "pptx", Target: "pdf"},
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub == "" {
			continue
		}
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	// minimal substring search to avoid an extra import
	if sub == "" {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}


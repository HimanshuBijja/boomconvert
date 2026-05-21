# CLAUDE.md — working notes for this project

> Context for future Claude sessions. This file is not meant for end users; the README is.

## What this project is

**BoomConvert.** A local-first file converter triggered by renaming the file's extension in Explorer. Go daemon + embedded HTML dashboard. See `prd.md` for full requirements and `README.md` for user-facing docs.

## Architectural decisions (so you don't re-litigate them)

- **Language: Go.** Considered Rust and Python. Go won because the goal is a lightweight always-on daemon; conversion ecosystem differences are mooted by the "wrap external CLIs" strategy.
- **No native Go conversion libs in conversion paths.** Go stdlib `image/*` only handles JPEG/PNG/GIF — far short of the "any-to-any" goal. We orchestrate ImageMagick / LibreOffice / pdf2docx / Poppler / unoserver instead.
- **SQLite via `modernc.org/sqlite`.** Pure Go, no CGO, single static binary preserved. JSON was rejected: bad for concurrent writes and history queries.
- **Backups outside the watched tree.** `%APPDATA%/BoomConvert/backups/`. Putting `.backup` inside the watched folder would create fsnotify feedback loops.
- **HTTP bound to `127.0.0.1` only.** Never `0.0.0.0`.
- **Adapter pattern with `LookupAll` + sequential fallback.** Replaced an earlier single-`Lookup` design. UnoAdapter is preferred when available; failures cascade to LibreOfficeAdapter. A two-failure circuit breaker disables UnoAdapter for the session on flaky machines.
- **Doc scope intentionally narrow for v1.** Only DOCX↔PDF, PPTX→PDF, PDF→DOCX. DOCX↔PPTX and PDF→PPTX are explicitly out of scope because no reliable open-source path exists; we surface "not supported" rather than producing junk.
- **Tool discovery is filesystem-aware.** Don't rely on `PATH` — Windows shells often have stale PATH after installs. We probe LookPath AND glob standard install locations (`C:\Program Files\ImageMagick-*`, `C:\Program Files\LibreOffice\program\`, `%APPDATA%\Python\Python*\Scripts\`, etc.) and prepend discovered tool dirs to our process's PATH.
- **`unoserver` must be invoked via LibreOffice's bundled Python.** The pip-generated `unoserver.exe`/`unoconvert.exe` wrappers have a malformed shebang (`#!"C:\Program Files\LibreOffice\program\python-core-3.12.13"` is a directory, not an executable). We bypass them by running `LO_PYTHON -m unoserver.server` and `LO_PYTHON -c "from unoserver.client import converter_main; ..."`.

## Status: what works, what's known-flaky

### Verified working end-to-end on Windows
- Any image-format pair (tested JPG↔PNG, ~100ms)
- DOCX → PDF (tested live, ~7-14s with direct soffice; ~1-2s warm with working unoserver)
- Magic-byte detection with OOXML zip-sniffer to disambiguate DOCX/PPTX/XLSX
- Atomic `*.bc-tmp.ext` writes
- Central backup directory + restore-on-failure
- HTTP REST + WebSocket dashboard (`/api/status`, `/api/tools`, `/api/folders`, `/api/history`, `/api/matrix`, `/api/analytics`, `/api/backups`, `/api/backups/:id/restore`, `/api/retention`, `/api/retention/sweep`, `/ws`)
- Backup browser + one-click restore in the dashboard (with optional "also delete converted")
- Backup retention policy: age-based + total-size cap, persisted in `settings`, swept hourly from `RunRetentionLoop`
- Parallel tool probes with tight version-probe timeout (2.5 s)
- LookupAll-based adapter fallback
- 2-failure circuit breaker on UnoAdapter

### Known flaky / unverified
- **`unoserver` on this dev machine specifically**: every conversion through it fails with `Looks like LibreOffice died. PID: ...` followed by a `PermissionError` cleaning up `%TEMP%\tmpXXXXXXXX\user\extensions\bundled\extensions.pmap`. Almost certainly antivirus interaction. The circuit breaker handles this gracefully (2 slow conversions, then disabled for session), so the daemon stays reliable.
- **PDF → DOCX via `pdf2docx`**: adapter is registered, tool is detected, but not yet exercised end-to-end in a test.
- **Cross-directory renames on Windows**: handled in design (REMOVE+CREATE correlation in `watcher.go`) but not explicitly tested.
- **Compound extensions** (`.tar.gz`): handled in design (only the last segment is the extension) but not specifically tested.

## File map (where the logic lives)

```
main.go              startup, wires Store + ToolHealth + Registry + Watcher + UnoManager + Converter + retention loop + Server
retention.go         RetentionPolicy, SweepBackups (age + size-cap pruning), RunRetentionLoop (hourly tick)
retention_test.go    unit tests for retention sweeper (age, size cap, missing dir, zero policy)
watcher_test.go      unit tests for detectFormatWithFallback, sniffOOXML disambiguation, normalizeExt, tempSibling
paths.go             OS-aware config + backup directory resolution
tool_health.go       parallel probes for magick/soffice/pdf2docx/unoserver/poppler
                     + prependDiscoveredDirsToPath so adapters can call by short name
store.go             SQLite tables: conversions, watched_folders, settings (JSON-blob values)
hub.go               WebSocket broadcast hub
watcher.go           fsnotify subscription, rename correlation (REMOVE/RENAME + CREATE pairs),
                     detectFormatWithFallback (filetype.Match + sniffOOXML + original-ext fallback),
                     internal-rename ignore map
converter.go         orchestrates a single conversion: backup -> LookupAll loop with fallback
                     -> atomic finalize -> record in DB -> broadcast event
                     tempSibling() preserves the target extension so format-by-ext tools work
                     RestoreBackup() copies a backup back to source path via atomic temp-sibling write
server.go            HTTP routes + WS upgrade
adapters/registry.go Adapter interface, Registry, LookupAll
adapters/image.go    ImageMagickAdapter (any-to-any 7 image formats)
adapters/document.go LibreOfficeAdapter (soffice --headless --convert-to in isolated profile),
                     Pdf2DocxAdapter (python -m pdf2docx convert)
adapters/uno.go      UnoManager (lazy-start unoserver, idle-timeout teardown, circuit breaker),
                     UnoAdapter (proxy through manager; tight 45s inner timeout)
adapters/image_test.go ImageMagickAdapter unit + integration tests (round-trip skips when magick absent)
frontend/index.html  dashboard markup (embedded via go:embed in main.go)
frontend/style.css   dark glassmorphic theme
frontend/app.js      polls /api/*, listens on /ws, handles folder/tool actions
```

## Important invariants to preserve

- **Never break the single-binary story.** No CGO. No external Go SQLite drivers. Keep using `modernc.org/sqlite`.
- **Atomic finalize.** Always convert to `tempSibling(target)` first, rename on success. Never write directly to the target path.
- **Backups are sacred.** Every conversion makes a timestamped copy in `%APPDATA%\BoomConvert\backups\` BEFORE running the adapter. Never delete a backup automatically without an explicit retention policy + user setting.
- **Watcher ignore map.** Any time we write a file that lives inside a watched directory, register it in `Watcher.ignored` for at least 30 s. Forgetting this creates infinite rename loops.
- **HTTP bound only to 127.0.0.1.** Don't change to `:8001` or `0.0.0.0:8001`.
- **Tool-availability gate.** `Registry.LookupAll` already skips adapters whose `RequiredTool()` isn't available. Don't bypass this.

## Pitfalls already paid for (don't reintroduce)

1. **Temp file extension matters.** First version used `target.png.bc.tmp` — ImageMagick saw the `.tmp` extension and didn't re-encode. Always use `tempSibling()` which preserves the real extension (`foo.bc-tmp.png`).
2. **Filetype detection is too generic for OOXML.** `h2non/filetype` reports `zip` for DOCX/PPTX/XLSX. Always go through `detectFormatWithFallback` → `sniffOOXML` which peeks at the zip entries.
3. **LibreOffice profile collisions.** Always pass `-env:UserInstallation=file:///<tempdir>` to `soffice` so concurrent / repeat invocations don't fight over a shared user profile.
4. **unoserver Windows wrappers are broken.** Pip-generated `.exe` shim has malformed shebang. Use `LO_PYTHON -m unoserver.server` and `LO_PYTHON -c "from unoserver.client import converter_main; ..."` instead.
5. **`soffice --version` on Windows hangs forever.** GUI-subsystem binaries don't return stdout to console parents. Tool Health uses a 2.5 s probe timeout and trusts filesystem existence alone if the version probe doesn't return.
6. **Serial probes are slow.** Always run tool probes in parallel goroutines.
7. **Don't kill the boomconvert process during tests with an in-flight LibreOffice subprocess.** It propagates context cancel down the tree and you get `exit status 143` (SIGTERM) on Linux/git-bash. Either wait for the conversion or kill more selectively.

## What to do next (priority-ordered)

### High value, low risk
1. **Smoke-test PDF → DOCX end-to-end.** Pdf2DocxAdapter is wired but unexercised. Drop a PDF into the Magic Folder, rename to `.docx`, verify a usable DOCX is produced. **Needs a human at the keyboard.**
2. **Run the dashboard in a browser interactively.** Click through Tool Health, folder manager, activity feed, **backup browser + restore + retention controls (new)**. Surface UI rough edges. **Needs a human at the keyboard.**

### Medium value
3. **Antivirus mitigation guidance.** If `unoserver` consistently dies on a user's machine, surface a banner in the dashboard pointing at the AV-exclusion troubleshooting in README. Hook point: detect when the UnoAdapter circuit breaker has tripped (see `adapters/uno.go`) and emit a hub event the dashboard can render.
4. **PPTX → PDF live test.** LibreOffice adapter supports it but it's never been exercised live.
5. **Rename-correlation integration tests.** The current `watcher_test.go` only covers pure helpers. A real fsnotify-driven test that simulates RENAME→CREATE pairs would catch regressions in cross-directory renames + compound-extension correlation.

### Lower priority
6. **Windows installer / single-click setup.** Inno Setup or MSI that bundles ImageMagick / LibreOffice install commands.
7. **Run as a Windows service.** Use `golang.org/x/sys/windows/svc` so BoomConvert starts on login without showing a console window.
8. **libvips swap for images.** ImageMagick is fine, but `vips` is ~4-8x faster on most operations and ~30 MB vs ~50 MB. Possible adapter variant.
9. **More format families.** Modern photo formats (HEIC, AVIF via ImageMagick + libheif), ebooks (Calibre), archives (7-Zip). Discussed and deferred from v1.

### Avoid / explicitly deferred
- Cloud APIs for the DOCX↔PPTX hard cases. Breaks the local-first principle.
- A `:0.0.0.0` listening mode for "share the dashboard". Local-only is a hard design constraint.

## How to verify the build quickly

```powershell
# Build
go build -o boomconvert.exe .

# Smoke test (clear DB + watch folder first)
Remove-Item "$env:APPDATA\BoomConvert\boomconvert.db*" -ErrorAction SilentlyContinue
Remove-Item "$env:USERPROFILE\BoomConvert-Magic\*" -ErrorAction SilentlyContinue

# Run, then in another shell:
#   Drop test.jpg into %USERPROFILE%\BoomConvert-Magic\
#   Rename test.jpg -> test.png
#   Verify with: file test.png  (should report PNG)
#   Verify dashboard at http://127.0.0.1:8001
```

## Useful operational commands

```powershell
# Force-stop a stuck binary
taskkill /F /IM boomconvert.exe

# Stop a runaway soffice spawned by unoserver
taskkill /F /IM soffice.bin
taskkill /F /IM python.exe

# Inspect the DB directly
modernc.org/sqlite has no CLI; use any sqlite tool against %APPDATA%\BoomConvert\boomconvert.db

# Live log tail (we don't write a log file by default — log goes to stdout)
.\boomconvert.exe 2>&1 | Tee-Object -FilePath bc.log
```

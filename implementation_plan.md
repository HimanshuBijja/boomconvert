# Implementation Plan: BoomConvert (Go Version)

Implement the core structure, file-watching service, conversion adapter system, and frontend dashboard for BoomConvert in Go.

## User Review Required

> [!IMPORTANT]
> **Go Toolchain**
> This implementation plan assumes Go (≥ 1.21) is installed locally. We will configure a `go.mod` file and write standard Go modules.

> [!WARNING]
> **External Conversion Tools**
> Conversions are performed by best-in-class external CLIs:
> - **ImageMagick** (`magick`) — images
> - **LibreOffice** (`soffice`) — DOCX↔PDF, PPTX↔PDF
> - **Python + `pdf2docx`** — PDF→DOCX
> - **Poppler** (`pdftoppm`) — PDF page rasterisation fallback
>
> All four are auto-detected at startup via a Tool Health probe. Missing tools do not prevent BoomConvert from starting; the dashboard surfaces which conversion rules are unlocked, and shows one-click install commands (`winget` / `choco` / `apt` / `brew`) for any missing tool.

---

## Open Questions

> [!NOTE]
> **Initial Watch Folder**
> By default we will watch a folder named `MagicConvert` inside the project workspace directory until the user adds their own from the dashboard.

---

## Proposed Changes

### Backend (Go Service)

A modular Go application with clear separation between watching, orchestration, adapters, and storage.

#### [NEW] [go.mod](file:///d:/code/file-change-by-extension/go.mod)
Module definition and direct dependencies:
- `github.com/fsnotify/fsnotify` — cross-platform filesystem notifications
- `github.com/gorilla/websocket` — dashboard real-time updates
- `github.com/h2non/filetype` — magic-byte signature detection
- `modernc.org/sqlite` — pure-Go SQLite (no CGO, single static binary)

#### [NEW] [main.go](file:///d:/code/file-change-by-extension/main.go)
Entry point:
1. Resolves config directory (`%APPDATA%/BoomConvert` on Windows, `~/.boomconvert` elsewhere).
2. Opens SQLite store via `store.go`.
3. Runs `tool_health.Probe()` synchronously; stores results.
4. Builds the adapter registry against detected tools.
5. Starts the file watcher.
6. Starts the HTTP server bound to **`127.0.0.1:8001`**, serving embedded dashboard assets via `go:embed`.
7. Upgrades the `/ws` endpoint to a WebSocket hub for dashboard events.

#### [NEW] [tool_health.go](file:///d:/code/file-change-by-extension/tool_health.go)
Detects external tools at startup and on demand:
- Probes each of `magick`, `soffice`, `python -m pdf2docx --version`, `pdftoppm -v`.
- Records: name, found (bool), path, version, raw probe output.
- Exposes `func Available(toolID string) bool` used by the adapter registry to gate rules.
- Exposes a `/api/tools` endpoint returning the latest probe.

#### [NEW] [store.go](file:///d:/code/file-change-by-extension/store.go)
SQLite persistence. WAL mode enabled.
Schema:
```sql
CREATE TABLE IF NOT EXISTS conversions (
    id INTEGER PRIMARY KEY,
    source_path TEXT, target_path TEXT,
    source_format TEXT, target_format TEXT,
    source_size INTEGER, target_size INTEGER,
    status TEXT,            -- 'completed' | 'failed' | 'reverted'
    error TEXT,
    started_at DATETIME, finished_at DATETIME,
    backup_path TEXT
);
CREATE TABLE IF NOT EXISTS watched_folders (
    id INTEGER PRIMARY KEY,
    path TEXT UNIQUE,
    enabled INTEGER,
    added_at DATETIME
);
CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT             -- JSON blob
);
```

#### [NEW] [adapters/registry.go](file:///d:/code/file-change-by-extension/adapters/registry.go)
- Defines `type Adapter interface { Convert(ctx, src, dst, opts) error; SourceFormats() []string; TargetFormats() []string; RequiredTool() string }`.
- The registry is built at startup from the list of adapters whose `RequiredTool()` is available.
- Provides `Lookup(src, dst string) (Adapter, bool)`.

#### [NEW] [adapters/image.go](file:///d:/code/file-change-by-extension/adapters/image.go)
Wraps `magick` CLI. Supports any-to-any across {JPEG, PNG, GIF, WebP, BMP, TIFF, ICO}.
- Builds command `magick <src> [-quality N] [-background white -alpha remove] <dst.tmp>`.
- Honours quality / lossless options from settings.
- Runs under `context.WithTimeout`.

#### [NEW] [adapters/document.go](file:///d:/code/file-change-by-extension/adapters/document.go)
Three sub-adapters:
- **LibreOfficeAdapter** — DOCX↔PDF, PPTX↔PDF. Runs `soffice --headless --convert-to <ext> --outdir <tmpdir> <src>` in an isolated profile dir to avoid clobbering user LibreOffice settings.
- **Pdf2DocxAdapter** — PDF→DOCX via `python -m pdf2docx convert <src> <dst.tmp>`.
- Adapter selection prioritises higher-fidelity tools (e.g. `pdf2docx` over LibreOffice for PDF→DOCX).

#### [NEW] [watcher.go](file:///d:/code/file-change-by-extension/watcher.go)
Directory monitoring and conversion orchestration:
1. Subscribes to `fsnotify` events for each enabled watched folder (recursive).
2. Correlates Windows cross-directory renames (REMOVE+CREATE pairs within a short window matching size/inode).
3. On detecting an extension change:
    - Reads magic bytes; if signature matches the new extension, no-op.
    - Copies original to `<backupDir>/<timestamp>_<originalName>`.
    - Looks up adapter; if none / tool unavailable, reverts the rename.
    - Invokes adapter writing to `<target>.tmp`, then atomically renames to `<target>`.
    - On failure, restores from backup and reverts the filename.
4. Maintains a mutex-protected `ignoredPaths` set with self-cleaning entries so the watcher does not re-process files it just wrote.

#### [NEW] [converter.go](file:///d:/code/file-change-by-extension/converter.go)
Thin orchestrator used by the watcher: lookup adapter → backup → convert → atomic rename → persist conversion record → emit WebSocket event.

#### [NEW] [server.go](file:///d:/code/file-change-by-extension/server.go)
HTTP + WebSocket layer:
- `GET /` — serves embedded `frontend/index.html`.
- `GET /api/status` — watcher state.
- `POST /api/status/toggle` — pause/resume.
- `GET /api/tools` — Tool Health snapshot.
- `GET /api/folders`, `POST /api/folders`, `DELETE /api/folders/:id`.
- `GET /api/history?limit=...` — recent conversions.
- `GET /api/matrix` — currently available conversion rules (computed from registry).
- `GET /api/backups`, `POST /api/backups/:id/restore`.
- `GET /ws` — WebSocket hub.

#### [NEW] [config defaults]
Bootstrapped on first run into the SQLite `settings` table — no separate `config.json` is needed.

---

### Frontend (Embedded Dashboard UI)

Served by Go on **`127.0.0.1:8001`**, embedded via `go:embed`.

#### [NEW] [frontend/index.html](file:///d:/code/file-change-by-extension/frontend/index.html)
Sections:
- Header: watcher status pill + pause/resume toggle.
- **Tool Health card** — per-tool status badges with install commands for missing tools.
- Magic folders panel — add/remove/disable.
- Conversion matrix card — auto-rendered from `/api/matrix`.
- Activity feed — live conversions with size delta + duration.
- Backup browser — list + restore.
- Analytics card — space saved, totals, format distribution.

#### [NEW] [frontend/style.css](file:///d:/code/file-change-by-extension/frontend/style.css)
Dark-mode glassmorphic layout:
- HSL CSS variables (obsidian black, violet accent, emerald success, coral error, amber warning for missing-tool states).
- `backdrop-filter: blur()` cards, smooth transitions, keyframe animations for in-progress conversions.

#### [NEW] [frontend/app.js](file:///d:/code/file-change-by-extension/frontend/app.js)
- Connects to `ws://localhost:8001/ws` on load.
- Fetches `/api/tools`, `/api/folders`, `/api/matrix`, `/api/history` on startup.
- Reacts to live WS messages (`conversion_started`, `conversion_completed`, `conversion_failed`, `tool_health_changed`).
- Handles folder add/remove, backup restore, settings updates.

---

## Verification Plan

### Automated Tests
- `watcher_test.go` — mock rename events, including Windows-style REMOVE+CREATE, case-insensitive extensions, compound extensions, signature mismatches.
- `adapters/image_test.go` — golden fixtures for JPEG↔PNG↔WebP round-trips (skipped if `magick` not present).
- `tool_health_test.go` — synthetic PATH manipulation to simulate missing tools.

### Manual Verification
1. `go build -o boomconvert.exe` produces a single static binary.
2. Run `boomconvert.exe` — observe Tool Health probe output in console.
3. Open `http://127.0.0.1:8001` — dashboard loads; Tool Health panel reflects installed tools.
4. Add a watched folder; drop `test.jpg` into it; rename to `test.png`.
   - File converts to true PNG.
   - Original is preserved in `%APPDATA%/BoomConvert/backups/`.
   - Dashboard activity feed updates live.
5. Rename `report.docx` → `report.pdf` (with LibreOffice installed) — produces a PDF.
6. With ImageMagick uninstalled, repeat (1) — dashboard shows missing tool and a winget install hint, rename is reverted, no junk file produced.

# BoomConvert

Rename a file's extension in your file explorer; BoomConvert converts the file in place.

`vacation.jpeg` → rename to `vacation.png` → it really becomes a PNG. Locally. No uploads.

A lightweight Go background service that watches "Magic Folders" for extension renames, verifies the file's true format by magic bytes, runs the conversion through best-in-class external tools (ImageMagick, LibreOffice, pdf2docx, optional unoserver), and exposes a real-time dashboard at `http://127.0.0.1:8001`.

---

## Features

- **Magic-rename trigger** — rename `report.docx` to `report.pdf` in Explorer and it converts.
- **Any-to-any image conversion** — JPEG, PNG, GIF, WebP, BMP, TIFF, ICO (42 pairs) via ImageMagick.
- **Office conversions** — DOCX ↔ PDF, PPTX → PDF via LibreOffice; PDF → DOCX via pdf2docx.
- **Optional keep-warm fast path** — `unoserver` cuts doc conversions from ~14 s to ~1-2 s.
- **Safety first** — every original is preserved in a central backup directory before conversion. Atomic writes mean a failed conversion never replaces your file with a partial one.
- **Graceful degradation** — auto-detects which tools are installed and only offers conversions it can actually perform. Missing tools surface in the dashboard with one-click install commands.
- **Auto-fallback + circuit breaker** — if the fast-path adapter fails twice it's disabled for the session and the reliable path takes over silently.
- **Single 16 MB binary** — pure-Go SQLite, no CGO, no extra runtimes for the daemon itself.
- **Local only** — HTTP server binds to `127.0.0.1`, never `0.0.0.0`.

---

## Quick start

### 1. Prerequisites

You need **Go 1.21+** to build. To get useful conversions you'll want at least one of:

| Tool | Required for | Install (Windows) |
|---|---|---|
| **ImageMagick** | All image conversions | `winget install ImageMagick.ImageMagick` |
| **LibreOffice** | DOCX/PPTX → PDF | `winget install TheDocumentFoundation.LibreOffice` |
| **pdf2docx** (Python) | PDF → DOCX | `pip install pdf2docx` |
| **unoserver** (optional, fast doc conversions) | Keep-warm DOCX/PPTX → PDF | `& "C:\Program Files\LibreOffice\program\python.exe" -m pip install unoserver` |

BoomConvert auto-discovers all of these on startup. None of them are hard requirements — the conversion matrix in the dashboard simply reflects what's installed.

### 2. Build

```powershell
git clone <this repo>
cd file-change-by-extension
go build -o boomconvert.exe .
```

You should get a single ~16 MB `boomconvert.exe`.

### 3. Run

```powershell
.\boomconvert.exe
```

On first launch BoomConvert:
- Creates its config dir at `%APPDATA%\BoomConvert\` (Windows) or `~/.boomconvert/` (Unix).
- Opens its SQLite store at `%APPDATA%\BoomConvert\boomconvert.db`.
- Seeds a default Magic Folder at `%USERPROFILE%\BoomConvert-Magic\`.
- Probes for installed conversion tools and prints which ones it found.
- Starts the dashboard at **<http://127.0.0.1:8001>**.

### 4. Convert a file

1. Drop any image, DOCX, PPTX, or PDF into your Magic Folder.
2. Rename the extension in Explorer (e.g. `holiday.jpg` → `holiday.png`).
3. Watch the dashboard's activity feed update live as the conversion runs.
4. The converted file replaces the renamed one. The original is preserved in `%APPDATA%\BoomConvert\backups\`.

---

## Dashboard

`http://127.0.0.1:8001`

- **Status pill + Pause/Resume** — temporarily halt all watchers.
- **Tool Health** — shows each external tool's detected version and path; missing tools show a copy-paste install command.
- **Magic Folders** — add, remove, or disable watched folders. New folders are watched recursively.
- **Conversion Matrix** — dynamic; only shows rules whose tools are present.
- **Activity** — live feed of in-progress and recent conversions with size delta and duration.

---

## Supported conversions (v1)

### Images (any-to-any)
JPEG · PNG · GIF · WebP · BMP · TIFF · ICO

### Documents
- DOCX → PDF
- PPTX → PDF
- PDF → DOCX

Out of scope for v1 (no reliable open-source path): DOCX ↔ PPTX, PDF → PPTX. The dashboard makes this explicit rather than silently producing low-fidelity output.

---

## Performance

| Conversion | Time |
|---|---|
| Image any-to-any | ~100 ms |
| DOCX/PPTX → PDF via `unoserver` (warm) | ~1–2 s |
| DOCX/PPTX → PDF via `unoserver` (cold first run) | ~5–10 s |
| DOCX/PPTX → PDF via direct `soffice` | ~7–14 s |

On machines where `unoserver` works, every doc conversion after the first is near-instant. On machines where `unoserver` has trouble (commonly antivirus interfering with its temp LibreOffice profile), BoomConvert silently falls back to the direct `soffice` path after detecting two consecutive failures, so you're never stuck.

---

## Troubleshooting

### Tool Health shows a tool as MISSING after I installed it
Newly-installed tools on Windows often aren't on existing shells' `PATH`. BoomConvert also probes standard install locations (`C:\Program Files\ImageMagick-*`, `C:\Program Files\LibreOffice\program\`, `%APPDATA%\Python\Python*\Scripts\`) — if the tool was installed elsewhere, click **Re-probe** in the dashboard or restart BoomConvert.

### LibreOffice conversions are slow / `unoserver` fails repeatedly
This is usually antivirus holding the LibreOffice temp profile open. Add `%TEMP%` and `%APPDATA%\Python` to your AV exclusions and click **Re-probe**. If it still fails, BoomConvert will automatically disable `unoserver` after two failures and use the slower (but reliable) direct `soffice` path.

### A conversion failed and my file disappeared
It hasn't — every original is copied to `%APPDATA%\BoomConvert\backups\<timestamp>_<filename>` before conversion. Open that folder to restore.

### Port 8001 is already in use
Another `boomconvert.exe` is probably still running. Use Task Manager (or `taskkill /F /IM boomconvert.exe`) to stop it.

---

## File layout

```
boomconvert.exe         single static binary (after go build)
go.mod / go.sum         module dependencies
main.go                 entry point, wires everything together
paths.go                resolves config / backup / DB paths per OS
tool_health.go          probes external tools at startup
store.go                SQLite (modernc.org/sqlite) schema + queries
watcher.go              fsnotify event loop + rename detection
converter.go            orchestrator: backup -> adapter loop -> atomic finalize
hub.go                  WebSocket broadcast hub
server.go               HTTP + WS routes
adapters/
  registry.go           Adapter interface, Lookup / LookupAll
  image.go              ImageMagick wrapper (any-to-any images)
  document.go           LibreOffice + pdf2docx wrappers
  uno.go                unoserver keep-warm + circuit breaker
frontend/
  index.html            dashboard markup (embedded via go:embed)
  style.css             dark-mode glassmorphic UI
  app.js                fetches /api/*, listens on /ws
prd.md                  product requirements
implementation_plan.md  implementation plan
CLAUDE.md               working notes for AI assistants
```

---

## License

(add your license here)

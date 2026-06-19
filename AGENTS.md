# AGENTS.md — w9y Codebase Guide

w9y is a WebAssembly blob hosting service with content-addressed storage, automatic gzip serving, and on-demand Go WASM builds. Written in Go, single package.

---

## Architecture

```
cmd/w9y/main.go         → entry point, calls w9y.Run()
w9y.go                  → CLI dispatcher, shared helpers (gzip, blob paths, mapping)
server.go               → HTTP handler (NewServer), upload/download/list/serve logic
upload.go               → CLI upload subcommand (client-side)
backup.go               → CLI backup subcommand (download all from remote)
restore.go              → CLI restore subcommand (upload all to remote)
gc.go                   → CLI GC subcommand (find/remove orphan blobs)
check.go                → CLI check subcommand (verify blob integrity)
gowasm.go               → /go/ endpoint: on-demand Go WASM builds via `go install`
w9y_test.go             → integration tests (httptest-based), unit tests, test helpers
```

Single Go package `w9y`. No internal packages, no subpackages, no router library — uses `net/http` directly.

## Commands

```sh
# Build
go build -o /bin/w9y ./cmd/w9y

# Run server (default port 8080 or $PORT)
go run ./cmd/w9y server
PORT=8080 DATA_DIR=/var/lib/w9y go run ./cmd/w9y server

# Run CLI commands against remote (default: https://w9y.up.railway.app/)
# Override with W9Y env var
W9Y=http://localhost:8080 go run ./cmd/w9y upload foo.wasm
W9Y=http://localhost:8080 go run ./cmd/w9y backup
W9Y=http://localhost:8080 go run ./cmd/w9y restore

# Run all tests
go test ./...

# Run specific test
go test -run TestUploadStoresBlobAndMappingEntry ./...
```

No Makefile, no CI configs, no task runner.

## Data Flow

### Upload
1. Client hashes the raw (uncompressed) wasm bytes → SHA-256
2. Client does `HEAD /blob/<sha256>.wasm.gz` to check if blob exists on server
3. If exists: sends `PUT /path.wasm?sha256=<sha>` with no body (link-only)
4. If not: gzips locally, sends `PUT /path.wasm?sha256=<sha>` with gzip body
5. Server stores gzip blob at `data/blob/<sha256>.wasm.gz`, creates symlink at `data/paths/<path>` → `../blob/<sha256>.wasm.gz`

### Download
1. Request hits `GET /path.wasm` → follows symlink `data/paths/<path>` to resolve SHA
2. Gets SHA from symlink target, opens `data/blob/<sha256>.wasm.gz`
3. Serves with `Content-Type: application/wasm`, `Content-Encoding: gzip`
4. **Blob paths** (`/blob/<sha>.wasm.gz`) served as raw gzip — no `Content-Encoding`, just raw bytes

### Go WASM (`/go/` paths)
1. `GET /go/<import-path>@<version>` → resolves version, builds via `go install` with `GOOS=js GOARCH=wasm`
2. Result stored as a regular blob in the store + symlink entry
3. Concurrent requests for the same build coordinate via a channel-based wait pattern (`buildOrWait`)
4. Non-canonical refs (commit hashes, branches) resolved to pseudo-versions via `go list -m -json`
5. No version / `@latest` → resolves latest version and redirects (302)

## Storage Layout (Symlinks)

```
data/
  paths/
    foo.wasm -> ../blob/<sha256>.wasm.gz    # entry symlink
    sub/
      bar.wasm -> ../../blob/<sha256>.wasm.gz
    go/
      pkg@v1.0.0 -> ../../../blob/<sha256>.wasm.gz
      pkg@latest -> pkg@v1.0.0              # alias symlink (points to another path)
  blob/
    <sha256>.wasm.gz  # gzip-compressed wasm binary
```

Each path is a symlink under `data/paths/`. Entries point to `../blob/<sha>.wasm.gz`;
aliases point to another relative path. The symlink's mtime (or a `.time` sidecar)
records the upload timestamp. Listing means walking the `data/paths/` tree.

## CLI Subcommands

All subcommands use Go's `flag` package with `flag.ContinueOnError`. Flag parsing stops after first positional arg — put flags **before** file args.

| Subcommand | Flags | Notes |
|---|---|---|
| `server` | `-port`, `-check` | Also reads `PORT` and `DATA_DIR` env vars |
| `upload` | `--to` | Single file arg. Client-side gzip + dedup |
| `backup` | `-data-dir` | Downloads entries + all blobs from remote |
| `restore` | `-data-dir` | Uploads entries + blobs to remote |
| `gc` | `-data-dir`, `-clean` | Dry-run by default; `-clean` deletes orphans |
| `check` | `-data-dir` | Validates SHA256 + WASM magic bytes for all blobs |

Remote host is set via `W9Y` env var (default: `https://w9y.up.railway.app/`).

## Key Patterns & Gotchas

### Content-Addressing
- **Hashes are computed over the raw (uncompressed) wasm bytes**, not the gzip bytes. This ensures dedup regardless of compression.
- The client hashes before gzipping; the server hashes by decompressing the uploaded gzip.

### Blob Path Reservation
- Uploads to `/blob` or `/blob/**` are rejected with 400. The `/blob` namespace is reserved for content-addressed storage.

### Gzip Serving
- `serveGzipFile` peeks at the first 2 bytes to detect gzip magic (`0x1f, 0x8b`). Only sets `Content-Encoding: gzip` when the file is actually gzip-compressed (not all stored blobs are guaranteed gzip — e.g., future non-wasm content).
- Blob paths (`/blob/<sha>.wasm.gz`) are served **raw** via `serveRawFile` — no `Content-Encoding` header. This is intentional: the client gets the gzip bytes as-is.
- Download of mapped paths uses `Vary: Accept-Encoding` but **always** serves the pre-gzipped file with `Content-Encoding: gzip` — no on-the-fly compression negotiation.

### Go WASM Build
- Uses `go install` (not `go build`), because `go install pkg@version` accepts the `@version` syntax but `go build` does not.
- Uses isolated temp `GOPATH` (set to tmpdir), cleaned up after build.
- Sets `GOWORK=off` for all `go` invocations to avoid workspace mode confusion.
- 5-minute timeout per build. Builds are deduplicated: concurrent requests for the same `remotePath` coordinate via channels — one builds, others wait.
- WASM output binary is found at `<GOPATH>/bin/js_wasm/<basename>` after `go install` with `GOOS=js GOARCH=wasm`.

### Test Patterns
- All tests in a single `w9y_test.go` file, same package (`package w9y`), so they can test unexported functions.
- Server tests use `httptest.NewRecorder` and `httptest.NewServer`.
- Client tests use a custom `roundTripFunc` type (implements `http.RoundTripper`) to mock the transport.
- Test helpers: `mustGzip(t, []byte)`, `mustGunzip(t, []byte)`, `roundTripFunc`, `textResponse(status, body)`.
- All tests use `t.TempDir()` for temp directories (auto-cleaned).
- Table-driven tests for parse functions (`TestParseGoWasmPath`).
- Naming: `Test<Feature><Scenario>` — e.g., `TestUploadStoresBlobAndMappingEntry`, `TestLinkOnlyRejectsMissingBlob`.
- `t.Fatal` / `t.Fatalf` used throughout (not `t.Error`).

### Shared Helpers (in `w9y.go`)
| Function | Purpose |
|---|---|
| `blobPath(dataDir, sha, gz)` | Returns `dataDir/blob/<sha>.wasm[.gz]` |
| `blobRemotePath(sha, gz)` | Returns `/blob/<sha>.wasm[.gz]` |
| `isBlobRemotePath(path)` | Returns true for `/blob` or `/blob/*` |
| `sha256Hex(body)` | SHA-256 of raw bytes as hex string |
| `contentType(path)` | Returns `application/wasm` for `.wasm`, otherwise uses `mime.TypeByExtension` |
| `blobSymlinkTarget`, `storePath` | Symlink path construction from remote path and hash |
| `gzipBytes(src)` | Gzip with `BestCompression` |
| `getenv(key, fallback)` | Env var with fallback |

### Naming Conventions
- Functions: `camelCase` (unexported) and `PascalCase` (exported — only `Run`, `NewServer`).
- Error messages: lowercase, no trailing punctuation.
- Variables: short, idiomatic Go (e.g., `sha` for hash, `gz` for gzip bytes, `gr` for gzip reader, `gzw` for gzip writer).
- Tests: `Test<Feature><Scenario>`.

### Other Gotchas
- `handleList` at `/` returns JSON sorted by time descending. All paths not in the `data/paths/` tree get 404.
- Upload uses `os.Rename` from temp file to final path — atomic on the same filesystem.
- `backup` creates a transport clone with `DisableCompression = true` so blob downloads aren't double-decompressed by the HTTP transport.
- `backup` deduplicates by SHA256 across entries (multiple paths can point to same blob).
- `restore` groups entries by SHA256 and reuses the same blob for multiple entries.
- The `check` command uses `strings.CutSuffix` to strip `.wasm.gz` — ensures blob naming compliance.
- Dockerfile uses `btwiuse/arch:golang` base image, not the official Go image.
- Go 1.26.4 (bleeding edge — may not exist outside this project's toolchain).
- `go.sum` is gitignored — not checked in.

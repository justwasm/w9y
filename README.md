# w9y

w9y is a WebAssembly blob hosting service with content-addressed storage, automatic gzip serving, on-demand Go WASM builds, and a Go module proxy for GitHub Gists.

Upload a `.wasm` file once, then serve any named `.wasm` path with gzip compression. Each path is a symlink under `data/paths/` pointing to a deduplicated blob store under `data/blob/`, so multiple public paths can point at the same uploaded bytes. CORS is enabled by default, and no authentication is required yet.

## Install

```sh
go install github.com/btwiuse/w9y/cmd/w9y@latest
```

## Server

Set `PORT` to start server mode:

```sh
PORT=8080 w9y
```

By default, files are stored in `./data`. Set `DATA_DIR` to use another directory:

```sh
PORT=8080 DATA_DIR=/var/lib/w9y w9y
```

Use `-check` to verify blob integrity at startup:

```sh
w9y server -check
```

## Storage Layout

Uploaded blobs are stored by the SHA-256 hash of the original wasm bytes. Paths are symlinks under `data/paths/` pointing into the blob store:

```text
data/
  paths/
    foo.wasm -> ../blob/<sha256>.wasm.gz  # entry
    sub/
      bar.wasm -> ../../blob/<sha256>.wasm.gz
    go/
      pkg@v1.0.0 -> ../../../blob/<sha256>.wasm.gz
      pkg@latest -> pkg@v1.0.0          # alias (points to another path)
  blob/
    <sha256>.wasm.gz
```

Each symlink's mtime (or a `.time` sidecar file) records the upload timestamp. Aliases are symlinks pointing to another path entry rather than to a blob. Listing entries means walking the `data/paths/` directory tree.

Uploads to `/blob` or `/blob/**` are rejected. The `/blob` namespace is reserved for content-addressed storage.

## Upload

Set `W9Y` to the remote w9y endpoint, then upload a file:

```sh
W9Y=http://localhost:8080 w9y upload foo.wasm
```

That uploads to `/foo.wasm`.

Use `--to` to choose a different remote path:

```sh
W9Y=http://localhost:8080 w9y upload --to /bar.wasm foo.wasm
```

`--to /bar.wasm` must come before the filepath. w9y uses Go's standard flag parser, which stops parsing flags after the first positional argument.

The client hashes the original wasm and checks `HEAD /blob/<sha256>.wasm.gz` first. If the blob already exists on the server, it sends a link-only PUT with the `?sha256=` query parameter and no body. Otherwise it sends the gzipped wasm as the body.

## Serving

After upload, request the `.wasm` path:

```sh
curl -I http://localhost:8080/foo.wasm
```

w9y serves the generated gzip blob with:

```http
Content-Type: application/wasm
Content-Encoding: gzip
Access-Control-Allow-Origin: *
```

The `/blob/<sha>.wasm.gz` path serves the raw gzip bytes (no `Content-Encoding`), suitable for direct download.

## Backup

Download all blobs and mapping from the remote server to a local directory:

```sh
w9y backup
```

By default, files are saved to `./data`. Use `-data-dir` to change the destination:

```sh
w9y backup -data-dir /tmp/mybackup
```

Or pass the directory as a positional argument:

```sh
w9y backup /tmp/mybackup
```

The backup preserves all blobs and the symlink entries. Progress is shown on stderr.

## Restore

Upload all blobs and symlink entries from a local backup to the remote server:

```sh
w9y restore
```

By default, reads from `./data`. Use `-data-dir` to change the source:

```sh
w9y restore -data-dir /tmp/mybackup
```

Or pass the directory as a positional argument:

```sh
w9y restore /tmp/mybackup
```

Progress is shown on stderr.

## GC (Garbage Collection)

Find blobs in storage that are not referenced by any path entry:

```sh
w9y gc
```

By default, runs in dry-run mode (lists orphans only). Use `-clean` to actually delete:

```sh
w9y gc -clean
```

## Check

Verify blob integrity by checking SHA-256 hashes and WASM magic bytes:

```sh
w9y check
```

## Build (Go WASM)

Build a Go WASM binary from any Go module with build-from-source semantics (honors `replace` directives in `go.mod`):

```sh
w9y build github.com/user/repo/cmd/app@latest
w9y build github.com/user/repo/cmd/app@v0.1.0
w9y build github.com/user/repo/cmd/app@commit-hash
```

The output is written to the current directory as `<app>.wasm`.

## Go WASM On-Demand (`/go/`)

The server can build and serve Go WASM binaries on demand. Request a module with an optional version:

```text
GET /go/<import-path>            → list available versions
GET /go/<import-path>@<version>  → build & serve WASM (Go)
```

Examples:

```sh
# Build and serve the latest version
curl -o app.wasm https://w9y.io/go/github.com/btwiuse/w9y/cmd/w9y@latest

# Build a specific version
curl -o app.wasm https://w9y.io/go/github.com/btwiuse/w9y/cmd/w9y@v0.1.0
```

The build process:
1. Resolves the version and downloads the module
2. Copies to a writable temp directory
3. Applies `replace` directives (clipboard fork, bubbletea fork)
4. Runs `go mod tidy`
5. Builds with `GOOS=js GOARCH=wasm`
6. Stores and serves the result

### Gist Support

GitHub Gists are supported as Go WASM builds. The server automatically clones the gist and runs the build pipeline:

```text
GET /go/gist.github.com/<user>/<id>          → resolve latest commit, redirect
GET /go/gist.github.com/<user>/<id>@<commit> → build & serve WASM
```

Example:

```sh
curl -o app.wasm https://w9y.io/go/gist.github.com/btwiuse/83e2efac358d22a009ece0a3f4feb801
```

If the gist has no `go.mod`, one is auto-initialized with the correct module path.

## TinyGo WASM On-Demand (`/tinygo/`)

Same as `/go/` but uses TinyGo for smaller WASM binaries:

```text
GET /tinygo/<import-path>@<version> → build & serve WASM (TinyGo)
```

TinyGo produces significantly smaller WASM binaries (typically 4-10x smaller than standard `go build`).

```sh
curl -o app.wasm https://w9y.io/tinygo/github.com/user/repo@latest
curl -o app.wasm https://w9y.io/tinygo/gist.github.com/user/id@commit
```

### wasm_exec.js

The JavaScript glue files needed to run WASM binaries in the browser are served at:

| Endpoint | Source |
|---|---|
| `/go/wasm_exec.js` | `$(go env GOROOT)/lib/wasm/wasm_exec.js` |
| `/tinygo/wasm_exec.js` | `$(tinygo env TINYGOROOT)/targets/wasm_exec.js` |

## Go Module Proxy (`/goproxy/`)

The server implements the Go module proxy protocol for gist paths, enabling `go get` / `go install` with gist modules:

```sh
GOPROXY=https://w9y.io/goproxy go get gist.github.com/<user>/<id>@<commit>
```

Standard goproxy endpoints:

| Endpoint | Description |
|---|---|
| `@v/list` | List versions (pseudo-version from commit hash) |
| `@v/<version>.info` | Version info JSON |
| `@v/<version>.mod` | Module `go.mod` |
| `@v/<version>.zip` | Module source zip |
| `@latest` | Latest version info |

The proxy also handles `?go-get=1` discovery, so gist modules can be resolved by the Go toolchain via direct VCS.

## CLI Commands Summary

| Command | Description |
|---|---|
| `server` | Start HTTP server |
| `upload` | Upload a wasm file |
| `backup` | Download all blobs from remote |
| `restore` | Upload all blobs to remote |
| `gc` | Find/remove orphan blobs |
| `check` | Verify blob integrity |
| `build` | Build Go WASM binary locally |
| `build` | Build Go WASM binary locally (use `--tinygo` for TinyGo) |
| `semver` | Check if a string is a valid semantic version |

## Docker

Build the image:

```sh
docker build -t w9y .
```

Run the server:

```sh
docker run --rm -p 8080:8080 -e PORT=8080 w9y
```

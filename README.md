# w9y

w9y is a WebAssembly blob hosting service with content-addressed storage and automatic gzip serving.

Upload a `.wasm` file once, then serve any named `.wasm` path with gzip compression. The path is mapped to a deduplicated blob store via `mapping.yaml`, so multiple public paths can point at the same uploaded bytes. CORS is enabled by default, and no authentication is required yet.

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

## Storage Layout

Uploaded blobs are stored by the SHA-256 hash of the original wasm bytes. A `mapping.yaml` file maps public paths to blob hashes:

```text
data/
  mapping.yaml
  blob/
    <sha256>.wasm.gz
```

```yaml
# mapping.yaml
entries:
  /foo.wasm:
    sha: abc123...
    time: 1718000000000
  /pkg/bar.wasm:
    sha: def456...
    time: 1718000001000
```

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

The backup preserves all blobs and the mapping file. Progress is shown on stderr.

## Restore

Upload all blobs and mapping from a local backup to the remote server:

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

## Docker

Build the image:

```sh
docker build -t w9y .
```

Run the server:

```sh
docker run --rm -p 8080:8080 -e PORT=8080 w9y
```

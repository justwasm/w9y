# w9y

w9y is a WebAssembly blob hosting service with content-addressed storage and automatic gzip serving.

Upload a `.wasm` file once, then serve any named `.wasm` path with gzip compression. The path is mapped to a deduplicated blob store via `mapping.yaml`, so multiple public paths can point at the same uploaded bytes. CORS is enabled by default, and no authentication is required yet.

## Build

```sh
go build ./cmd/w9y
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
/foo.wasm: abc123...
/pkg/bar.wasm: def456...
```

Uploads to `/blob` or `/blob/**` are rejected. The `/blob` namespace is reserved for content-addressed storage.

## Upload

Set `HOST` to the remote w9y endpoint, then upload a file:

```sh
HOST=http://localhost:8080 w9y upload foo.wasm
```

That uploads to `/foo.wasm`.

Use `--to` to choose a different remote path:

```sh
HOST=http://localhost:8080 w9y upload --to /bar.wasm foo.wasm
```

`--to /bar.wasm` must come before the filepath. w9y uses Go's standard flag parser, which stops parsing flags after the first positional argument.

The client gzips the wasm file and sends it as the PUT body. No custom headers or pre-flight checks are needed.

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

Create a tarball from the data directory:

```sh
w9y backup backup.tar
```

Use a custom data directory:

```sh
w9y backup --data-dir /var/lib/w9y backup.tar
```

The backup preserves all blobs and the mapping file.

## Restore

Restore a tarball into the data directory:

```sh
w9y restore backup.tar
```

Use a custom data directory:

```sh
w9y restore --data-dir /var/lib/w9y backup.tar
```

## Docker

Build the image:

```sh
docker build -t w9y .
```

Run the server:

```sh
docker run --rm -p 8080:8080 -e PORT=8080 w9y
```

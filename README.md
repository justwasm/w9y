# w9y

w9y is a WebAssembly blob hosting service with automatic gzip compression.

Upload a `.wasm` file once, then serve the original `.wasm` path with gzip compression for free. Uploads overwrite existing files, CORS is enabled by default, and no authentication is required yet.

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

## Upload

Set `HOST` to the remote w9y endpoint, then upload a file:

```sh
HOST=http://localhost:8080 w9y upload foo.wasm
```

That uploads to `/foo.wasm`, stores the original file, and generates `/foo.wasm.gz`.

Use `--to` to choose a different remote path:

```sh
HOST=http://localhost:8080 w9y upload --to /bar.wasm foo.wasm
```

`--to /bar.wasm` must come before the filepath. w9y uses Go's standard flag parser, which stops parsing flags after the first positional argument.

## Serving

After upload, request the `.wasm` path:

```sh
curl -I http://localhost:8080/foo.wasm
```

w9y serves the generated gzip file with:

```http
Content-Type: application/wasm
Content-Encoding: gzip
Access-Control-Allow-Origin: *
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

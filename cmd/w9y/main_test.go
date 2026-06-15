package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestUploadGeneratesGzipAndServesWasmWithGzipHeaders(t *testing.T) {
	dir := t.TempDir()
	server := newServer(dir)

	body := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	req := httptest.NewRequest(http.MethodPut, "/foo.wasm", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "foo.wasm.gz")); err != nil {
		t.Fatalf("expected gzip file: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/foo.wasm", nil)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/wasm" {
		t.Fatalf("Content-Type = %q, want application/wasm", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("CORS header = %q, want *", got)
	}

	gz, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	decompressed, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("decompress response: %v", err)
	}
	if !bytes.Equal(decompressed, body) {
		t.Fatalf("decompressed body = %v, want %v", decompressed, body)
	}
}

func TestCleanRemotePath(t *testing.T) {
	tests := map[string]string{
		"foo.wasm":      "/foo.wasm",
		"/foo.wasm":     "/foo.wasm",
		"/dir/foo.wasm": "/dir/foo.wasm",
	}
	for input, want := range tests {
		got, err := cleanRemotePath(input)
		if err != nil {
			t.Fatalf("cleanRemotePath(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("cleanRemotePath(%q) = %q, want %q", input, got, want)
		}
	}
}

package w9y

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadStoresBlobAndMappingEntry(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	body := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	sha := sha256Hex(body)
	req := httptest.NewRequest(http.MethodPut, "/foo.wasm", bytes.NewReader(mustGzip(t, body)))
	req.Header.Set(headerBlobSHA256, sha)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "blob", sha+".wasm.gz")); err != nil {
		t.Fatalf("expected gzip blob: %v", err)
	}

	mapping, err := loadMapping(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mapping["/foo.wasm"] != sha {
		t.Fatalf("mapping = %v, want /foo.wasm -> %s", mapping, sha)
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
	if got := mustGunzip(t, rec.Body.Bytes()); !bytes.Equal(got, body) {
		t.Fatalf("decompressed body = %v, want %v", got, body)
	}
}

func TestMultiplePathsMapToSameBlob(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	body := []byte("wasm")
	sha := sha256Hex(body)

	req := httptest.NewRequest(http.MethodPut, "/one.wasm", bytes.NewReader(mustGzip(t, body)))
	req.Header.Set(headerBlobSHA256, sha)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d", rec.Code, http.StatusCreated)
	}

	req = httptest.NewRequest(http.MethodPut, "/two.wasm", http.NoBody)
	req.Header.Set(headerBlobSHA256, sha)
	req.Header.Set(headerLinkOnly, "1")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	mapping, err := loadMapping(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mapping["/one.wasm"] != sha {
		t.Fatalf("mapping /one.wasm = %q, want %q", mapping["/one.wasm"], sha)
	}
	if mapping["/two.wasm"] != sha {
		t.Fatalf("mapping /two.wasm = %q, want %q", mapping["/two.wasm"], sha)
	}
}

func TestUploadRejectsBlobDestination(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)
	body := []byte("wasm")

	req := httptest.NewRequest(http.MethodPut, "/blob/manual.wasm", bytes.NewReader(mustGzip(t, body)))
	req.Header.Set(headerBlobSHA256, sha256Hex(body))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("upload status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestBlobPathServesDirectly(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)
	body := []byte("wasm")
	sha := sha256Hex(body)

	req := httptest.NewRequest(http.MethodPut, "/foo.wasm", bytes.NewReader(mustGzip(t, body)))
	req.Header.Set(headerBlobSHA256, sha)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d", rec.Code, http.StatusCreated)
	}

	req = httptest.NewRequest(http.MethodGet, blobRemotePath(sha, true), nil)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("blob download status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestCleanUploadPathRejectsBlob(t *testing.T) {
	if _, err := cleanUploadPath("/blob/foo.wasm"); err == nil {
		t.Fatal("cleanUploadPath(/blob/foo.wasm) returned nil error")
	}
	if got, err := cleanUploadPath("foo.wasm"); err != nil || got != "/foo.wasm" {
		t.Fatalf("cleanUploadPath(foo.wasm) = %q, %v; want /foo.wasm, nil", got, err)
	}
}

func TestUploadClientChecksBlobAndSendsLinkOnlyWhenPresent(t *testing.T) {
	body := []byte{0x00, 0x61, 0x73, 0x6d}
	file := filepath.Join(t.TempDir(), "foo.wasm")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sha := sha256Hex(body)

	var requests []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.Path)
		switch {
		case req.Method == http.MethodHead && req.URL.Path == blobRemotePath(sha, true):
			return textResponse(http.StatusOK, ""), nil
		case req.Method == http.MethodPut && req.URL.Path == "/bar.wasm":
			if req.Header.Get(headerLinkOnly) != "1" {
				t.Fatalf("link-only header = %q, want 1", req.Header.Get(headerLinkOnly))
			}
			gotBody, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if len(gotBody) != 0 {
				t.Fatalf("link-only body length = %d, want 0", len(gotBody))
			}
			return textResponse(http.StatusCreated, "uploaded /bar.wasm"), nil
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})}
	t.Setenv("HOST", "http://example.test")

	if err := uploadWithClient([]string{"--to", "/bar.wasm", file}, client); err != nil {
		t.Fatal(err)
	}
	want := []string{"HEAD " + blobRemotePath(sha, true), "PUT /bar.wasm"}
	if strings.Join(requests, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
}

func TestUploadClientSendsCompressedBlobWhenMissing(t *testing.T) {
	body := []byte("wasm")
	file := filepath.Join(t.TempDir(), "foo.wasm")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sha := sha256Hex(body)

	var uploaded []byte
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodHead:
			return textResponse(http.StatusNotFound, ""), nil
		case http.MethodPut:
			if req.Header.Get(headerBlobSHA256) != sha {
				t.Fatalf("sha header = %q, want %q", req.Header.Get(headerBlobSHA256), sha)
			}
			if req.Header.Get(headerLinkOnly) != "" {
				t.Fatalf("link-only header = %q, want empty", req.Header.Get(headerLinkOnly))
			}
			var err error
			uploaded, err = io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			return textResponse(http.StatusCreated, "uploaded /foo.wasm"), nil
		default:
			t.Fatalf("unexpected method %s", req.Method)
			return nil, nil
		}
	})}
	t.Setenv("HOST", "http://example.test")

	if err := uploadWithClient([]string{file}, client); err != nil {
		t.Fatal(err)
	}
	if got := mustGunzip(t, uploaded); !bytes.Equal(got, body) {
		t.Fatalf("uploaded gzip body decompresses to %q, want %q", got, body)
	}
}

func TestBackupAndRestorePreservesData(t *testing.T) {
	dir := t.TempDir()
	body := []byte("wasm")
	sha := sha256Hex(body)
	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob", sha+".wasm.gz"), mustGzip(t, body), 0o644); err != nil {
		t.Fatal(err)
	}
	mapping := map[string]string{"/foo.wasm": sha}
	if err := saveMapping(dir, mapping); err != nil {
		t.Fatal(err)
	}

	tarball := filepath.Join(t.TempDir(), "backup.tar")
	if err := backupData(dir, tarball); err != nil {
		t.Fatal(err)
	}

	restoreDir := t.TempDir()
	if err := restoreData(restoreDir, tarball); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(restoreDir, "blob", sha+".wasm.gz")); err != nil || !bytes.Equal(got, mustGzip(t, body)) {
		t.Fatalf("restored blob = %v, %v", got, err)
	}
	restoredMapping, err := loadMapping(restoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if restoredMapping["/foo.wasm"] != sha {
		t.Fatalf("restored mapping = %v, want /foo.wasm -> %s", restoredMapping, sha)
	}
}

func TestSafeTarPathRejectsTraversal(t *testing.T) {
	if _, err := safeTarPath(t.TempDir(), "../outside"); err == nil {
		t.Fatal("safeTarPath accepted traversal")
	}
}

func TestNotFoundReturns404(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent.wasm", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func mustGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	gz, err := gzipBytes(body)
	if err != nil {
		t.Fatal(err)
	}
	return gz
}

func mustGunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	decompressed, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("decompress response: %v", err)
	}
	return decompressed
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func textResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

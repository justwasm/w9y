package w9y

import (
	"archive/tar"
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

func TestUploadStoresContentAddressedBlobAndServesSymlinkPath(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(dir, "blob", sha+".wasm")); err != nil {
		t.Fatalf("expected wasm blob: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blob", sha+".wasm.gz")); err != nil {
		t.Fatalf("expected gzip blob: %v", err)
	}
	link, err := os.Readlink(filepath.Join(dir, "foo.wasm"))
	if err != nil {
		t.Fatalf("expected symlink: %v", err)
	}
	if link != filepath.Join("blob", sha+".wasm") {
		t.Fatalf("symlink = %q, want %q", link, filepath.Join("blob", sha+".wasm"))
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

func TestNestedUploadUsesRelativeSymlink(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)
	body := []byte("wasm")
	sha := sha256Hex(body)

	req := httptest.NewRequest(http.MethodPut, "/a/b/foo.wasm", bytes.NewReader(mustGzip(t, body)))
	req.Header.Set(headerBlobSHA256, sha)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	link, err := os.Readlink(filepath.Join(dir, "a", "b", "foo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("..", "..", "blob", sha+".wasm")
	if link != want {
		t.Fatalf("symlink = %q, want %q", link, want)
	}
}

func TestLinkOnlyUploadUsesExistingBlob(t *testing.T) {
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
	link, err := os.Readlink(filepath.Join(dir, "two.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	if link != filepath.Join("blob", sha+".wasm") {
		t.Fatalf("symlink = %q, want blob link", link)
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

func TestBackupSkipsOriginalBlobAndRestoreRebuildsIt(t *testing.T) {
	dir := t.TempDir()
	body := []byte("wasm")
	sha := sha256Hex(body)
	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob", sha+".wasm"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob", sha+".wasm.gz"), mustGzip(t, body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("blob", sha+".wasm"), filepath.Join(dir, "foo.wasm")); err != nil {
		t.Fatal(err)
	}

	tarball := filepath.Join(t.TempDir(), "backup.tar")
	if err := backupData(dir, tarball); err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, tarball)
	if contains(names, "blob/"+sha+".wasm") {
		t.Fatalf("backup included original wasm blob: %v", names)
	}
	if !contains(names, "blob/"+sha+".wasm.gz") {
		t.Fatalf("backup missing gzip blob: %v", names)
	}
	if !contains(names, "foo.wasm") {
		t.Fatalf("backup missing symlink path: %v", names)
	}

	restoreDir := t.TempDir()
	if err := restoreData(restoreDir, tarball); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(restoreDir, "blob", sha+".wasm")); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("restored wasm = %q, %v; want %q, nil", got, err, body)
	}
	link, err := os.Readlink(filepath.Join(restoreDir, "foo.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	if link != filepath.Join("blob", sha+".wasm") {
		t.Fatalf("restored link = %q, want blob link", link)
	}
}

func TestSafeTarPathRejectsTraversal(t *testing.T) {
	if _, err := safeTarPath(t.TempDir(), "../outside"); err == nil {
		t.Fatal("safeTarPath accepted traversal")
	}
}

func TestValidateSymlinkTargetRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	linkPath := filepath.Join(dir, "a", "b", "foo.wasm")
	if err := validateSymlinkTarget(dir, linkPath, "../../blob/hash.wasm"); err != nil {
		t.Fatalf("validateSymlinkTarget rejected in-tree target: %v", err)
	}
	if err := validateSymlinkTarget(dir, linkPath, "../../../outside"); err == nil {
		t.Fatal("validateSymlinkTarget accepted escaping target")
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

func tarNames(t *testing.T, tarball string) []string {
	t.Helper()
	file, err := os.Open(tarball)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var names []string
	tr := tar.NewReader(file)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

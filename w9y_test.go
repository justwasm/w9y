package w9y

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
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
	gz := mustGzip(t, body)
	sha := sha256Hex(gz)

	req := httptest.NewRequest(http.MethodPut, "/foo.wasm", bytes.NewReader(gz))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "blob", sha+".wasm.gz")); err != nil {
		t.Fatalf("expected gzip blob: %v", err)
	}

	m, err := loadMapping(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := m.Entries["/foo.wasm"]
	if !ok || e.SHA != sha {
		t.Fatalf("mapping /foo.wasm = %+v, want sha=%s", e, sha)
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

func TestLinkOnlyViaQueryParam(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	body := []byte("wasm")
	gz := mustGzip(t, body)
	sha := sha256Hex(gz)

	// First upload: store the blob
	req := httptest.NewRequest(http.MethodPut, "/one.wasm", bytes.NewReader(gz))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload /one.wasm status = %d, want %d", rec.Code, http.StatusCreated)
	}

	// Second upload: link-only via query param, no body
	req = httptest.NewRequest(http.MethodPut, "/two.wasm?sha="+sha, http.NoBody)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link /two.wasm status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	m, err := loadMapping(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Entries["/one.wasm"].SHA != sha {
		t.Fatalf("mapping /one.wasm sha = %q, want %q", m.Entries["/one.wasm"].SHA, sha)
	}
	if m.Entries["/two.wasm"].SHA != sha {
		t.Fatalf("mapping /two.wasm sha = %q, want %q", m.Entries["/two.wasm"].SHA, sha)
	}

	// Both paths should serve the same content
	for _, path := range []string{"/one.wasm", "/two.wasm"} {
		req = httptest.NewRequest(http.MethodGet, path, nil)
		rec = httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get %s status = %d", path, rec.Code)
		}
		if got := mustGunzip(t, rec.Body.Bytes()); !bytes.Equal(got, body) {
			t.Fatalf("get %s body = %v, want %v", path, got, body)
		}
	}
}

func TestLinkOnlyRejectsMissingBlob(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	req := httptest.NewRequest(http.MethodPut, "/foo.wasm?sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", http.NoBody)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestUploadRejectsBlobDestination(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)
	gz := mustGzip(t, []byte("wasm"))

	req := httptest.NewRequest(http.MethodPut, "/blob/manual.wasm", bytes.NewReader(gz))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("upload status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestBlobPathServesDirectly(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)
	gz := mustGzip(t, []byte("wasm"))
	sha := sha256Hex(gz)

	req := httptest.NewRequest(http.MethodPut, "/foo.wasm", bytes.NewReader(gz))
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

func TestRootListsEntriesSortedByTime(t *testing.T) {
	dir := t.TempDir()
	m := &mapping{Entries: map[string]entry{
		"/a.wasm": {SHA: "a", Time: 100},
		"/b.wasm": {SHA: "b", Time: 200},
	}}
	if err := saveMapping(dir, m); err != nil {
		t.Fatal(err)
	}

	server := NewServer(dir)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("listing status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var items []struct {
		Path string `json:"path"`
		SHA  string `json:"sha"`
		Time int64  `json:"time"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("listing count = %d, want 2", len(items))
	}
	if items[0].Path != "/b.wasm" || items[0].Time != 200 {
		t.Fatalf("first item = %+v, want path=/b.wasm time=200", items[0])
	}
	if items[1].Path != "/a.wasm" || items[1].Time != 100 {
		t.Fatalf("second item = %+v, want path=/a.wasm time=100", items[1])
	}
}

func TestClientUploadWithPrecheck(t *testing.T) {
	body := []byte("wasm")
	sha := sha256Hex(body)

	var requests []struct{ method, path, query string }
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, struct{ method, path, query string }{
			req.Method, req.URL.Path, req.URL.RawQuery,
		})
		switch req.Method {
		case http.MethodHead:
			return textResponse(http.StatusOK, ""), nil
		case http.MethodPut:
			if req.Body != http.NoBody {
				t.Fatal("expected no body for existing blob")
			}
			return textResponse(http.StatusCreated, "uploaded /bar.wasm"), nil
		}
		return nil, nil
	})}

	t.Setenv("HOST", "http://example.test")
	file := filepath.Join(t.TempDir(), "foo.wasm")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := uploadWithClient([]string{"--to", "/bar.wasm", file}, client); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	if requests[0].method != "HEAD" || requests[0].path != blobRemotePath(sha, true) {
		t.Fatalf("request 0 = %s %s, want HEAD %s", requests[0].method, requests[0].path, blobRemotePath(sha, true))
	}
	if requests[1].method != "PUT" || requests[1].path != "/bar.wasm" || requests[1].query != "sha="+sha {
		t.Fatalf("request 1 = %s %s?%s, want PUT /bar.wasm?sha=%s", requests[1].method, requests[1].path, requests[1].query, sha)
	}
}

func TestClientUploadSendsBodyWhenBlobMissing(t *testing.T) {
	body := []byte("wasm")
	gz := mustGzip(t, body)
	sha := sha256Hex(body)

	var requests []struct{ method, path string }
	var uploaded []byte
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, struct{ method, path string }{req.Method, req.URL.Path})
		switch req.Method {
		case http.MethodHead:
			return textResponse(http.StatusNotFound, ""), nil
		case http.MethodPut:
			uploaded, _ = io.ReadAll(req.Body)
			return textResponse(http.StatusCreated, "uploaded /foo.wasm"), nil
		}
		return nil, nil
	})}

	t.Setenv("HOST", "http://example.test")
	file := filepath.Join(t.TempDir(), "foo.wasm")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := uploadWithClient([]string{file}, client); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	if requests[0].method != "HEAD" || requests[0].path != blobRemotePath(sha, true) {
		t.Fatalf("request 0 = %s %s, want HEAD %s", requests[0].method, requests[0].path, blobRemotePath(sha, true))
	}
	if requests[1].method != "PUT" || requests[1].path != "/foo.wasm" {
		t.Fatalf("request 1 = %s %s, want PUT /foo.wasm", requests[1].method, requests[1].path)
	}
	if !bytes.Equal(uploaded, gz) {
		t.Fatal("uploaded body does not match gzipped content")
	}
}

func TestBackupRemoteDownloadsEntriesAndBlobs(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	body := []byte("wasm")
	gz := mustGzip(t, body)
	sha := sha256Hex(gz)

	for _, path := range []string{"/foo.wasm", "/bar.wasm"} {
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(gz))
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("upload %s status = %d", path, rec.Code)
		}
	}

	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	destDir := t.TempDir()
	if err := backupRemote(http.DefaultClient, httpServer.URL, destDir); err != nil {
		t.Fatal(err)
	}

	m, err := loadMapping(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(m.Entries))
	}

	blob, err := os.ReadFile(filepath.Join(destDir, "blob", sha+".wasm.gz"))
	if err != nil {
		t.Fatalf("missing blob: %v", err)
	}
	if !bytes.Equal(blob, gz) {
		t.Fatal("blob content mismatch")
	}
}

func TestRestoreRemoteUploadsEntriesAndBlobs(t *testing.T) {
	backupDir := t.TempDir()
	body := []byte("wasm")
	gz := mustGzip(t, body)
	sha := sha256Hex(gz)

	if err := os.MkdirAll(filepath.Join(backupDir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "blob", sha+".wasm.gz"), gz, 0o644); err != nil {
		t.Fatal(err)
	}
	m := &mapping{Entries: map[string]entry{
		"/foo.wasm": {SHA: sha, Time: 100},
		"/bar.wasm": {SHA: sha, Time: 200},
	}}
	if err := saveMapping(backupDir, m); err != nil {
		t.Fatal(err)
	}

	serverDir := t.TempDir()
	server := NewServer(serverDir)
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	if err := restoreRemote(http.DefaultClient, httpServer.URL, backupDir); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/foo.wasm", "/bar.wasm"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("get %s status = %d", path, rec.Code)
		}
		if got := mustGunzip(t, rec.Body.Bytes()); !bytes.Equal(got, body) {
			t.Fatalf("get %s body = %v, want %v", path, got, body)
		}
	}

	entries, err := os.ReadDir(filepath.Join(serverDir, "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 blob file, got %d", len(entries))
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	body := []byte("wasm")
	gz := mustGzip(t, body)
	sha := sha256Hex(gz)

	req := httptest.NewRequest(http.MethodPut, "/foo.wasm", bytes.NewReader(gz))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", rec.Code)
	}

	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	backupDir := t.TempDir()
	if err := backupRemote(http.DefaultClient, httpServer.URL, backupDir); err != nil {
		t.Fatal(err)
	}

	serverDir2 := t.TempDir()
	server2 := NewServer(serverDir2)
	httpServer2 := httptest.NewServer(server2)
	defer httpServer2.Close()

	if err := restoreRemote(http.DefaultClient, httpServer2.URL, backupDir); err != nil {
		t.Fatal(err)
	}

	restored, err := loadMapping(serverDir2)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Entries["/foo.wasm"].SHA != sha {
		t.Fatalf("restored mapping = %+v", restored.Entries)
	}
	if _, err := os.Stat(filepath.Join(serverDir2, "blob", sha+".wasm.gz")); err != nil {
		t.Fatalf("missing blob: %v", err)
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

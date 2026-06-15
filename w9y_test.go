package w9y

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
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
	sha := sha256Hex(body)

	req := httptest.NewRequest(http.MethodPut, "/foo.wasm", bytes.NewReader(gz))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, "blob", sha+".wasm.gz")); err != nil {
		t.Fatalf("expected gzip blob: %v", err)
	}

	store := NewBlobStore(dir)
	e, err := store.Get("/foo.wasm")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if e.Hash != sha {
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
	sha := sha256Hex(body)

	// First upload: store the blob
	req := httptest.NewRequest(http.MethodPut, "/one.wasm", bytes.NewReader(gz))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload /one.wasm status = %d, want %d", rec.Code, http.StatusCreated)
	}

	// Second upload: link-only via query param, no body
	req = httptest.NewRequest(http.MethodPut, "/two.wasm?hash="+sha, http.NoBody)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("link /two.wasm status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	store := NewBlobStore(dir)
	e, err := store.Get("/one.wasm")
	if err != nil {
		t.Fatalf("store.Get /one.wasm: %v", err)
	}
	if e.Hash != sha {
		t.Fatalf("mapping /one.wasm sha = %q, want %q", e.Hash, sha)
	}
	e, err = store.Get("/two.wasm")
	if err != nil {
		t.Fatalf("store.Get /two.wasm: %v", err)
	}
	if e.Hash != sha {
		t.Fatalf("mapping /two.wasm sha = %q, want %q", e.Hash, sha)
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

	req := httptest.NewRequest(http.MethodPut, "/foo.wasm?hash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", http.NoBody)
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

func TestUploadRejectsHashMismatch(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	body := []byte("wasm")
	gz := mustGzip(t, body)
	wrongSHA := sha256Hex([]byte("not the actual content"))

	req := httptest.NewRequest(http.MethodPut, "/foo.wasm?hash="+wrongSHA, bytes.NewReader(gz))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	// Verify blob was not stored
	store := NewBlobStore(dir)
	if _, err := store.Get("/foo.wasm"); !errors.Is(err, ErrPathNotFound) {
		t.Fatal("mapping should not exist for rejected upload")
	}
}

func TestBlobPathServesDirectly(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)
	gz := mustGzip(t, []byte("wasm"))
	sha := sha256Hex([]byte("wasm"))

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

	blobDir := filepath.Join(dir, "blob")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, "a.wasm.gz"), bytes.Repeat([]byte("x"), 1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, "b.wasm.gz"), bytes.Repeat([]byte("y"), 1024*1024*2), 0o644); err != nil {
		t.Fatal(err)
	}

	store := NewBlobStore(dir)
	if err := store.Set("/a.wasm", Blob{Hash: "a", Time: 100}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("/b.wasm", Blob{Hash: "b", Time: 200}); err != nil {
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
		Hash string `json:"hash"`
		Time string `json:"time"`
		Size string `json:"size"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&items); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("listing count = %d, want 2", len(items))
	}
	if items[0].Path != "/b.wasm" || items[0].Time != "1970-01-01T00:00:00.2Z" {
		t.Fatalf("first item = %+v, want path=/b.wasm time=1970-01-01T00:00:00.2Z", items[0])
	}
	if items[1].Path != "/a.wasm" || items[1].Time != "1970-01-01T00:00:00.1Z" {
		t.Fatalf("second item = %+v, want path=/a.wasm time=1970-01-01T00:00:00.1Z", items[1])
	}
	if items[0].Size != "2.00MB" {
		t.Fatalf("first item size = %q, want 2.00MB", items[0].Size)
	}
	if items[1].Size != "1.00MB" {
		t.Fatalf("second item size = %q, want 1.00MB", items[1].Size)
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
	if requests[1].method != "PUT" || requests[1].path != "/bar.wasm" || requests[1].query != "hash="+sha {
		t.Fatalf("request 1 = %s %s?%s, want PUT /bar.wasm?hash=%s", requests[1].method, requests[1].path, requests[1].query, sha)
	}
}

func TestClientUploadSendsBodyWhenBlobMissing(t *testing.T) {
	body := []byte("wasm")
	gz := mustGzip(t, body)
	sha := sha256Hex(body)

	var requests []struct{ method, path, query string }
	var uploaded []byte
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, struct{ method, path, query string }{req.Method, req.URL.Path, req.URL.RawQuery})
		switch req.Method {
		case http.MethodHead:
			return textResponse(http.StatusNotFound, ""), nil
		case http.MethodPut:
			uploaded, _ = io.ReadAll(req.Body)
			return textResponse(http.StatusCreated, "uploaded /foo.wasm"), nil
		}
		return nil, nil
	})}

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
	if requests[1].method != "PUT" || requests[1].path != "/foo.wasm" || requests[1].query != "hash="+sha {
		t.Fatalf("request 1 = %s %s?%s, want PUT /foo.wasm?hash=%s", requests[1].method, requests[1].path, requests[1].query, sha)
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
	sha := sha256Hex(body)

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

	store := NewBlobStore(destDir)
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
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
	sha := sha256Hex(body)

	if err := os.MkdirAll(filepath.Join(backupDir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "blob", sha+".wasm.gz"), gz, 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewBlobStore(backupDir)
	if err := store.Set("/foo.wasm", Blob{Hash: sha, Time: 100}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("/bar.wasm", Blob{Hash: sha, Time: 200}); err != nil {
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
	sha := sha256Hex(body)

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

	store := NewBlobStore(serverDir2)
	restored, err := store.Get("/foo.wasm")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Hash != sha {
		t.Fatalf("restored mapping = %+v", restored)
	}
	if _, err := os.Stat(filepath.Join(serverDir2, "blob", sha+".wasm.gz")); err != nil {
		t.Fatalf("missing blob: %v", err)
	}
}

func TestGCDryRunPrintsOrphans(t *testing.T) {
	dir := t.TempDir()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob", sha+".wasm.gz"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No mapping → all blobs are orphans
	if err := gcData(dir, false); err != nil {
		t.Fatal(err)
	}

	// Blob should still exist (dry run)
	if _, err := os.Stat(filepath.Join(dir, "blob", sha+".wasm.gz")); err != nil {
		t.Fatal("dry run should not delete blobs")
	}
}

func TestGCCleanRemovesOrphans(t *testing.T) {
	dir := t.TempDir()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob", sha+".wasm.gz"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := gcData(dir, true); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "blob", sha+".wasm.gz")); !os.IsNotExist(err) {
		t.Fatal("clean should delete orphaned blobs")
	}
}

func TestGCLeavesReferencedBlobs(t *testing.T) {
	dir := t.TempDir()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob", sha+".wasm.gz"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewBlobStore(dir)
	if err := store.Set("/foo.wasm", Blob{Hash: sha, Time: 100}); err != nil {
		t.Fatal(err)
	}

	if err := gcData(dir, true); err != nil {
		t.Fatal(err)
	}

	// Referenced blob should still exist even with -clean
	if _, err := os.Stat(filepath.Join(dir, "blob", sha+".wasm.gz")); err != nil {
		t.Fatal("referenced blobs should not be removed")
	}
}

func TestGCWithNoBlobDir(t *testing.T) {
	dir := t.TempDir()
	if err := gcData(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := gcData(dir, true); err != nil {
		t.Fatal(err)
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

func TestParseGoWasmPath(t *testing.T) {
	tests := []struct {
		remotePath     string
		wantImportPath string
		wantVersion    string
		wantOK         bool
	}{
		{"/go/github.com/btwiuse/w9y/cmd/w9y@v0.1.0", "github.com/btwiuse/w9y/cmd/w9y", "v0.1.0", true},
		{"/go/github.com/btwiuse/w9y/cmd/w9y@latest", "github.com/btwiuse/w9y/cmd/w9y", "latest", true},
		{"/go/github.com/btwiuse/w9y/cmd/w9y", "github.com/btwiuse/w9y/cmd/w9y", "", true},
		{"/go/github.com/btwiuse/w9y", "github.com/btwiuse/w9y", "", true},
		{"/go/", "", "", false},
		{"/go/@v0.1.0", "", "", false}, // empty import path
	}
	for _, tt := range tests {
		got, ok := parseGoWasmPath(tt.remotePath)
		if ok != tt.wantOK {
			t.Errorf("parseGoWasmPath(%q) ok = %v, want %v", tt.remotePath, ok, tt.wantOK)
		}
		if got.ImportPath != tt.wantImportPath {
			t.Errorf("parseGoWasmPath(%q) ImportPath = %q, want %q", tt.remotePath, got.ImportPath, tt.wantImportPath)
		}
		if got.Version != tt.wantVersion {
			t.Errorf("parseGoWasmPath(%q) Version = %q, want %q", tt.remotePath, got.Version, tt.wantVersion)
		}
	}
}

func TestGoWasmPathWithNoVersionRedirectsToLatest(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	req := httptest.NewRequest(http.MethodGet, "/go/github.com/btwiuse/w9y", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	// Should redirect (302) to latest version, or fall back to listing (200)
	if rec.Code != http.StatusFound && rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 302 or 200", rec.Code)
	}
	if rec.Code == http.StatusFound {
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, "/go/github.com/btwiuse/w9y@") {
			t.Fatalf("Location = %q, want /go/github.com/btwiuse/w9y@...", loc)
		}
	}
}

func TestGoWasmPathWithLatestRedirectsToVersion(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	req := httptest.NewRequest(http.MethodGet, "/go/github.com/btwiuse/w9y@latest", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound && rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 302 or 200", rec.Code)
	}
}

func TestGoWasmPathReturns404ForBadPath(t *testing.T) {
	dir := t.TempDir()
	server := NewServer(dir)

	// Empty import path
	req := httptest.NewRequest(http.MethodGet, "/go/", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestVerifyBlob(t *testing.T) {
	dir := t.TempDir()
	body := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00} // valid wasm
	gz := mustGzip(t, body)
	sha := sha256Hex(body)

	blobDir := filepath.Join(dir, "blob")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gzPath := filepath.Join(blobDir, sha+".wasm.gz")
	if err := os.WriteFile(gzPath, gz, 0o644); err != nil {
		t.Fatal(err)
	}

	// Valid blob should pass
	if err := verifyBlob(gzPath, sha); err != nil {
		t.Fatalf("verifyBlob: %v", err)
	}

	// Wrong hash should fail
	if err := verifyBlob(gzPath, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("expected error for wrong hash")
	}

	// Corrupted content should fail
	corrupt := []byte("not gzip at all")
	corruptPath := filepath.Join(blobDir, "corrupt.wasm.gz")
	if err := os.WriteFile(corruptPath, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyBlob(corruptPath, sha); err == nil {
		t.Fatal("expected error for corrupt content")
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

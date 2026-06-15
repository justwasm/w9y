package w9y

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// NewServer returns the w9y HTTP handler.
func NewServer(dataDir string) http.Handler {
	store := NewBlobStore(dataDir)
	builder := NewGoWasmBuilder(store)
	return withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		remotePath, err := cleanRemotePath(r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if remotePath == "/" {
			handleList(w, store, dataDir)
			return
		}

		// Go WASM: build-on-demand from import path
		if isGoWasmPath(remotePath) {
			handleGoWasm(w, r, builder, remotePath)
			return
		}

		switch r.Method {
		case http.MethodPut, http.MethodPost:
			handleUpload(w, r, store, dataDir, remotePath)
		case http.MethodGet, http.MethodHead:
			handleDownload(w, r, store, dataDir, remotePath)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding")
		next.ServeHTTP(w, r)
	})
}

func handleList(w http.ResponseWriter, store BlobStore, dataDir string) {
	entries, err := store.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type item struct {
		Path  string `json:"path"`
		Hash  string `json:"hash"`
		Time  string `json:"time"`
		Size  string `json:"size"`
		epoch int64
	}
	items := make([]item, 0, len(entries))
	for p, e := range entries {
		t := time.UnixMilli(e.Time).UTC().Format(time.RFC3339Nano)
		fi, err := os.Stat(blobPath(dataDir, e.Hash, true))
		var s string
		if err == nil {
			s = fmt.Sprintf("%.2fMB", float64(fi.Size())/(1024*1024))
		} else {
			s = "0.00MB"
		}
		items = append(items, item{Path: p, Hash: e.Hash, Time: t, Size: s, epoch: e.Time})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].epoch > items[j].epoch
	})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(enc)
}

func handleUpload(w http.ResponseWriter, r *http.Request, store BlobStore, dataDir, remotePath string) {
	if isBlobRemotePath(remotePath) {
		http.Error(w, "uploads to /blob are not allowed", http.StatusBadRequest)
		return
	}

	claimed := r.URL.Query().Get("hash")

	// Fast path: hash provided and blob already exists — verify and link
	if claimed != "" {
		gzPath := blobPath(dataDir, claimed, true)
		if _, err := os.Stat(gzPath); err == nil {
			if err := verifyGzipHash(gzPath, claimed); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeUploadResponse(w, store, remotePath, claimed, true)
			return
		} else if !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Stream body to temp file in the blob directory
	gzDir := filepath.Dir(blobPath(dataDir, "x", true))
	if err := os.MkdirAll(gzDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20) // 100 MB gzip body limit
	tmpName, written, tmpErr := streamToTemp(gzDir, "*.wasm.gz", r.Body)
	if tmpErr != nil {
		http.Error(w, tmpErr.Error(), http.StatusBadRequest)
		return
	}
	if written == 0 {
		os.Remove(tmpName)
		http.Error(w, "blob not found", http.StatusNotFound)
		return
	}

	// Compute SHA-256 of the decompressed content
	sha, err := gzipSHA256(tmpName)
	if err != nil {
		os.Remove(tmpName)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// If hash was claimed, verify it matches
	if claimed != "" && claimed != sha {
		os.Remove(tmpName)
		http.Error(w, fmt.Sprintf("hash mismatch: got %s, want %s", sha, claimed), http.StatusBadRequest)
		return
	}

	gzPath := blobPath(dataDir, sha, true)

	// If target already exists, just clean up the temp file
	linked := false
	if _, err := os.Stat(gzPath); err == nil {
		os.Remove(tmpName)
	} else if os.IsNotExist(err) {
		if err := os.Rename(tmpName, gzPath); err != nil {
			os.Remove(tmpName)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		os.Remove(tmpName)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeUploadResponse(w, store, remotePath, sha, linked)
}

func writeUploadResponse(w http.ResponseWriter, store BlobStore, remotePath, sha string, linked bool) {
	if err := store.Set(remotePath, sha); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	verb := "uploaded"
	if linked {
		verb = "linked"
	}
	fmt.Fprintf(w, "%s %s -> blob/%s.wasm.gz", verb, remotePath, sha)
}

func handleDownload(w http.ResponseWriter, r *http.Request, store BlobStore, dataDir, remotePath string) {
	// blob path serves the gzip file as-is (raw bytes, no Content-Encoding)
	if isBlobRemotePath(remotePath) {
		gzPath := storagePath(dataDir, remotePath)
		serveRawFile(w, r, gzPath)
		return
	}

	e, err := store.Get(remotePath)
	if errors.Is(err, ErrPathNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gzPath := blobPath(dataDir, e.Hash, true)
	serveGzipFile(w, r, gzPath)
}

func serveRawFile(w http.ResponseWriter, r *http.Request, path string) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType(path))
	http.ServeContent(w, r, path, stat.ModTime(), file)
}

func serveGzipFile(w http.ResponseWriter, r *http.Request, gzPath string) {
	file, err := os.Open(gzPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Peek at first two bytes to detect gzip magic (0x1f, 0x8b).
	// Only set Content-Encoding: gzip when the file is actually gzip compressed.
	var header [2]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	file.Seek(0, io.SeekStart)

	virtualPath := strings.TrimSuffix(gzPath, ".gz")
	w.Header().Set("Content-Type", contentType(virtualPath))
	w.Header().Set("Vary", "Accept-Encoding")
	if header[0] == 0x1f && header[1] == 0x8b {
		w.Header().Set("Content-Encoding", "gzip")
	}
	http.ServeContent(w, r, path.Base(virtualPath), stat.ModTime(), file)
}

// verifyGzipHash opens the gzip file at path, decompresses it, and checks
// whether the SHA256 of the decompressed content matches wantSHA.
func verifyGzipHash(gzPath, wantSHA string) error {
	gotSHA, err := gzipSHA256(gzPath)
	if err != nil {
		return err
	}
	if gotSHA != wantSHA {
		return fmt.Errorf("hash mismatch: got %s, want %s", gotSHA, wantSHA)
	}
	return nil
}

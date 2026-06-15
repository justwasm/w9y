package w9y

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
			handleList(w, dataDir)
			return
		}

		// Go WASM: build-on-demand from import path
		if isGoWasmPath(remotePath) {
			handleGoWasm(w, r, dataDir, remotePath)
			return
		}

		switch r.Method {
		case http.MethodPut, http.MethodPost:
			handleUpload(w, r, dataDir, remotePath)
		case http.MethodGet, http.MethodHead:
			handleDownload(w, r, dataDir, remotePath)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		next.ServeHTTP(w, r)
	})
}

func handleList(w http.ResponseWriter, dataDir string) {
	m, err := loadMapping(dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type item struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Time   string `json:"time"`
		Size   string `json:"size"`
		epoch  int64
	}
	items := make([]item, 0, len(m.Entries))
	for p, e := range m.Entries {
		t := time.UnixMilli(e.Time).UTC().Format(time.RFC3339Nano)
		fi, err := os.Stat(blobPath(dataDir, e.SHA, true))
		var s string
		if err == nil {
			s = fmt.Sprintf("%.2fMB", float64(fi.Size())/(1024*1024))
		} else {
			s = "0.00MB"
		}
		items = append(items, item{Path: p, SHA256: e.SHA, Time: t, Size: s, epoch: e.Time})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].epoch > items[j].epoch
	})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc, _ := json.MarshalIndent(items, "", "  ")
	w.Write(enc)
}

func handleUpload(w http.ResponseWriter, r *http.Request, dataDir, remotePath string) {
	if isBlobRemotePath(remotePath) {
		http.Error(w, "uploads to /blob are not allowed", http.StatusBadRequest)
		return
	}

	var sha string
	linked := false
	if s := r.URL.Query().Get("sha256"); s != "" {
		sha = s
		gzPath := blobPath(dataDir, sha, true)
		if _, err := os.Stat(gzPath); os.IsNotExist(err) {
			// Blob not yet stored — stream body to temp file, then rename
			if err := os.MkdirAll(filepath.Dir(gzPath), 0o755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			tmp, tmpErr := os.CreateTemp(filepath.Dir(gzPath), "*.wasm.gz")
			if tmpErr != nil {
				http.Error(w, tmpErr.Error(), http.StatusInternalServerError)
				return
			}
			tmpName := tmp.Name()
			defer os.Remove(tmpName)
			written, copyErr := io.Copy(tmp, r.Body)
			tmp.Close()
			if copyErr != nil {
				http.Error(w, copyErr.Error(), http.StatusBadRequest)
				return
			}
			if written == 0 {
				http.Error(w, "blob not found", http.StatusNotFound)
				return
			}
			if err := os.Rename(tmpName, gzPath); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else {
			linked = true
		}
	} else {
		tmp, tmpErr := os.CreateTemp(dataDir, "*.wasm.gz")
		if tmpErr != nil {
			http.Error(w, tmpErr.Error(), http.StatusInternalServerError)
			return
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		written, copyErr := io.Copy(tmp, r.Body)
		tmp.Close()
		if copyErr != nil {
			http.Error(w, copyErr.Error(), http.StatusBadRequest)
			return
		}
		if written == 0 {
			http.Error(w, "blob not found", http.StatusNotFound)
			return
		}
		// Hash the decompressed (raw wasm) content for consistent content addressing
		f, openErr := os.Open(tmpName)
		if openErr != nil {
			http.Error(w, openErr.Error(), http.StatusInternalServerError)
			return
		}
		gr, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			f.Close()
			http.Error(w, gzErr.Error(), http.StatusBadRequest)
			return
		}
		h := sha256.New()
		io.Copy(h, gr)
		gr.Close()
		f.Close()
		sha = hex.EncodeToString(h.Sum(nil))
		gzPath := blobPath(dataDir, sha, true)
		if err := os.MkdirAll(filepath.Dir(gzPath), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmpName, gzPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	m, err := loadMapping(dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.Entries[remotePath] = entry{SHA: sha, Time: time.Now().UnixMilli()}
	if err := saveMapping(dataDir, m); err != nil {
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

func handleDownload(w http.ResponseWriter, r *http.Request, dataDir, remotePath string) {
	// blob path serves the gzip file directly
	if isBlobRemotePath(remotePath) {
		gzPath := storagePath(dataDir, remotePath)
		serveGzipFile(w, r, gzPath)
		return
	}

	m, err := loadMapping(dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	e, ok := m.Entries[remotePath]
	if !ok {
		http.NotFound(w, r)
		return
	}
	gzPath := blobPath(dataDir, e.SHA, true)
	serveGzipFile(w, r, gzPath)
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
	http.ServeContent(w, r, path.Base(virtualPath)+".gz", stat.ModTime(), file)
}

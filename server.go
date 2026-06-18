package w9y

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
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

		// Serve wasm_exec.js glue files
	if remotePath == goWasmPrefix+"wasm_exec.js" {
		serveWasmExecJS(w, r, "go")
		return
	}
	if remotePath == tinyGoWasmPrefix+"wasm_exec.js" {
		serveWasmExecJS(w, r, "tinygo")
		return
	}

	// Serve version info
	if remotePath == goWasmPrefix+"version" {
		serveVersion(w, r, "go")
		return
	}
	if remotePath == tinyGoWasmPrefix+"version" {
		serveVersion(w, r, "tinygo")
		return
	}

	// Go WASM: build-on-demand from import path
		if isGoWasmPath(remotePath) {
			handleGoWasm(w, r, builder, remotePath)
			return
		}

		// TinyGo WASM: build-on-demand from import path
		if isTinyGoWasmPath(remotePath) {
			handleTinyGoWasm(w, r, builder, remotePath)
			return
		}

		// Go module proxy for gist paths
		if isGoproxyPath(remotePath) {
			handleGoproxy(w, r, remotePath)
			return
		}

		// go-get=1 discovery for gist paths
		if r.URL.Query().Has("go-get") {
			handleGoGet(w, r, remotePath)
			return
		}

		switch r.Method {
		case http.MethodPut, http.MethodPost:
			handleUpload(w, r, store, dataDir, remotePath)
		case http.MethodGet, http.MethodHead:
			handleDownload(w, r, store, dataDir, remotePath)
		case http.MethodDelete:
			handleDelete(w, store, remotePath)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, POST, DELETE, OPTIONS")
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
	slices.SortFunc(items, func(a, b item) int {
		return cmp.Compare(b.epoch, a.epoch)
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
		serveFile(w, r, gzPath, false)
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
	serveFile(w, r, gzPath, true)
}

func handleDelete(w http.ResponseWriter, store BlobStore, remotePath string) {
	if isBlobRemotePath(remotePath) {
		http.Error(w, "cannot delete blob paths directly", http.StatusBadRequest)
		return
	}

	if err := store.Delete(remotePath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "deleted %s", remotePath)
}

func serveFile(w http.ResponseWriter, r *http.Request, filePath string, mightBeGzip bool) {
	f, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	virtualPath := filePath
	if mightBeGzip {
		virtualPath = strings.TrimSuffix(filePath, ".gz")
		w.Header().Set("Vary", "Accept-Encoding")
		var header [2]byte
		if _, err := io.ReadFull(f, header[:]); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.Seek(0, io.SeekStart)
		if header[0] == 0x1f && header[1] == 0x8b {
			w.Header().Set("Content-Encoding", "gzip")
		}
	}

	w.Header().Set("Content-Type", contentType(virtualPath))
	http.ServeContent(w, r, filepath.Base(virtualPath), stat.ModTime(), f)
}

// resolveWasmExecJS resolves the path to wasm_exec.js for the given runtime.
func resolveWasmExecJS(runtime string) (string, error) {
	switch runtime {
	case "go":
		goroot, err := exec.Command("go", "env", "GOROOT").Output()
		if err != nil {
			return "", fmt.Errorf("go env GOROOT: %w", err)
		}
		return filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm", "wasm_exec.js"), nil
	case "tinygo":
		tinygoroot, err := exec.Command("tinygo", "env", "TINYGOROOT").Output()
		if err != nil {
			return "", fmt.Errorf("tinygo env TINYGOROOT: %w", err)
		}
		return filepath.Join(strings.TrimSpace(string(tinygoroot)), "targets", "wasm_exec.js"), nil
	default:
		return "", fmt.Errorf("unknown runtime: %s", runtime)
	}
}

// serveWasmExecJS serves the wasm_exec.js glue file for the given runtime.
var wasmExecJSCache sync.Map // runtime → path

func serveWasmExecJS(w http.ResponseWriter, r *http.Request, runtime string) {
	path, ok := wasmExecJSCache.Load(runtime)
	if !ok {
		resolved, err := resolveWasmExecJS(runtime)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		wasmExecJSCache.Store(runtime, resolved)
		path = resolved
	}

	f, err := os.Open(path.(string))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/javascript")
	http.ServeContent(w, r, "wasm_exec.js", stat.ModTime(), f)
}

var versionCache sync.Map // runtime → string

func serveVersion(w http.ResponseWriter, r *http.Request, runtime string) {
	v, ok := versionCache.Load(runtime)
	if !ok {
		cmd := exec.Command(runtime, "version")
		out, err := cmd.Output()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		versionCache.Store(runtime, string(out))
		v = string(out)
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(v.(string)))
}

func newServerCommand() *cobra.Command {
	var (
		port    string
		doCheck bool
	)
	cmd := &cobra.Command{
		Use:   "server [-port port] [-check]",
		Short: "Start the w9y HTTP server",
		RunE: func(c *cobra.Command, args []string) error {
			addr := ":" + port
			slog.Info("starting server", "data_dir", dataDir, "addr", addr)

			if doCheck {
				if err := checkData(dataDir); err != nil {
					slog.Warn("integrity check found issues", "error", err)
				}
			}

			return http.ListenAndServe(addr, NewServer(dataDir))
		},
	}
	cmd.Flags().StringVar(&port, "port", cmp.Or(os.Getenv("PORT"), "8080"), "server port")
	cmd.Flags().BoolVar(&doCheck, "check", false, "run blob integrity check on startup")
	return cmd
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

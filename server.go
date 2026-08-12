package w9y

import (
	"cmp"
	"context"
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
	jobs := NewJobStore()
	mstore := NewManifestStore(dataDir)
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

		// API routes
		if remotePath == "/api/entries" {
			handleList(w, store, dataDir)
			return
		}
		if remotePath == "/api/build" && r.Method == http.MethodPost {
			handleBuildStart(w, r, builder, jobs)
			return
		}
		if strings.HasPrefix(remotePath, "/api/build/") && r.Method == http.MethodGet {
			jobID := strings.TrimPrefix(remotePath, "/api/build/")
			handleBuildPoll(w, jobID, jobs)
			return
		}
		if strings.HasPrefix(remotePath, "/api/auth/") {
			switch remotePath {
			case "/api/auth/login":
				handleAuthLogin(w, r)
			case "/api/auth/callback":
				handleAuthCallback(w, r)
			case "/api/auth/logout":
				handleAuthLogout(w, r)
			case "/api/auth/user":
				handleAuthUser(w, r)
			default:
				http.NotFound(w, r)
			}
			return
		}

		// Manifest API routes
		if strings.HasPrefix(remotePath, "/api/manifest") {
			handleManifestAPI(w, r, mstore, remotePath)
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
			if r.Method == http.MethodDelete {
				if authEnabled() && getUsername(r) == "" {
					http.Error(w, "login required", http.StatusUnauthorized)
					return
				}
				handleDelete(w, store, remotePath)
				return
			}
			if r.Method == http.MethodPut || r.Method == http.MethodPost {
				handleUpload(w, r, store, dataDir, remotePath)
				return
			}
			handleGoWasm(w, r, builder, remotePath)
			return
		}

		// TinyGo WASM: build-on-demand from import path
		if isTinyGoWasmPath(remotePath) {
			if r.Method == http.MethodDelete {
				if authEnabled() && getUsername(r) == "" {
					http.Error(w, "login required", http.StatusUnauthorized)
					return
				}
				handleDelete(w, store, remotePath)
				return
			}
			if r.Method == http.MethodPut || r.Method == http.MethodPost {
				handleUpload(w, r, store, dataDir, remotePath)
				return
			}
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

		// Manifest entry paths: /manifest/<name>@<ver>/<output>
		if strings.HasPrefix(remotePath, "/manifest/") {
			handleManifestEntry(w, r, mstore, builder, remotePath)
			return
		}

		switch r.Method {
		case http.MethodPut, http.MethodPost:
			handleUpload(w, r, store, dataDir, remotePath)
		case http.MethodGet, http.MethodHead:
			if remotePath == "/" {
				webHandler().ServeHTTP(w, r)
				return
			}
			// Serve other embedded web assets (e.g. /css/foo.css, /index.html).
			if webHasFile(remotePath) {
				webHandler().ServeHTTP(w, r)
				return
			}
			handleDownload(w, r, store, dataDir, remotePath)
		case http.MethodDelete:
			if authEnabled() && getUsername(r) == "" {
				http.Error(w, "login required", http.StatusUnauthorized)
				return
			}
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

	aliases, err := store.ListAliases()
	if err != nil {
		aliases = make(map[string]string)
	}

	type item struct {
		Path   string `json:"path"`
		Hash   string `json:"hash"`
		Time   string `json:"time"`
		Size   string `json:"size"`
		Alias  string `json:"alias,omitempty"`
		epoch  int64
	}
	items := make([]item, 0, len(entries)+len(aliases))
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
	for alias, target := range aliases {
		// Resolve the target to get hash/time/size
		if e, err := store.Get(target); err == nil {
			t := time.UnixMilli(e.Time).UTC().Format(time.RFC3339Nano)
			fi, err := os.Stat(blobPath(dataDir, e.Hash, true))
			var s string
			if err == nil {
				s = fmt.Sprintf("%.2fMB", float64(fi.Size())/(1024*1024))
			} else {
				s = "0.00MB"
			}
			items = append(items, item{Path: alias, Hash: e.Hash, Time: t, Size: s, Alias: target, epoch: e.Time})
		}
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

	// Alias creation — no blob upload needed
	if aliasTarget := r.URL.Query().Get("alias"); aliasTarget != "" {
		if err := store.SetAlias(remotePath, aliasTarget); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, "alias %s -> %s", remotePath, aliasTarget)
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

func handleManifestAPI(w http.ResponseWriter, r *http.Request, mstore *ManifestStore, remotePath string) {
	trimmed := strings.TrimPrefix(remotePath, "/api/manifest")
	trimmed = strings.TrimPrefix(trimmed, "/")

	if trimmed == "" {
		// GET /api/manifest — list all
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		manifests, err := mstore.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, manifests)
		return
	}

	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	name, version := parts[0], parts[1]

	switch r.Method {
	case http.MethodGet:
		data, err := mstore.Get(name, version)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(data)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Validate by parsing
		if _, err := ParseManifest("manifest", body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := mstore.Set(name, version, body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "stored manifest %s@%s", name, version)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleManifestEntry serves a WASM blob from a manifest entry.
// URL: /manifest/<name>@<version>/<output>
func handleManifestEntry(w http.ResponseWriter, r *http.Request, mstore *ManifestStore, builder *GoWasmBuilder, remotePath string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	trimmed := strings.TrimPrefix(remotePath, "/manifest/")

	// /manifest/ — root listing of all manifests
	if trimmed == "" {
		manifests, err := mstore.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!DOCTYPE html><html><head><meta charset=utf-8><title>manifests</title></head><body>\n")
		fmt.Fprintf(w, "<h1>manifests</h1>\n<ul>\n")
		// Sort names for stable output
		names := make([]string, 0, len(manifests))
		for n := range manifests {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, name := range names {
			for _, ver := range manifests[name] {
				fmt.Fprintf(w, "<li><a href=\"/manifest/%s@%s/\">%s@%s</a></li>\n", name, ver, name, ver)
			}
		}
		fmt.Fprintf(w, "</ul>\n</body></html>\n")
		return
	}

	// /manifest/<name>@<ver>/ — entry listing
	if trimmed[len(trimmed)-1] == '/' {
		name, version, ok := parseManifestRef(strings.TrimRight(trimmed, "/"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		data, err := mstore.Get(name, version)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m, err := ParseManifest("manifest", data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!DOCTYPE html><html><head><meta charset=utf-8><title>%s@%s</title></head><body>\n", name, version)
		fmt.Fprintf(w, "<h1><a href=\"/manifest/\">manifests</a> / %s@%s</h1>\n<ul>\n", name, version)
		for _, e := range m.Entries {
			ver := e.Version
			if ver == "" {
				ver = version
			}
			fmt.Fprintf(w, "<li><a href=\"/manifest/%s@%s/%s\">%s</a> → <a href=\"/go/%s@%s\">%s@%s</a></li>\n",
				name, version, e.Output, e.Output, e.Source, ver, e.Source, ver)
		}
		fmt.Fprintf(w, "</ul>\n</body></html>\n")
		return
	}

	// Entry redirect: /manifest/<name>@<ver>/<output>
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}

	firstPart := parts[0]
	outputPath := parts[1]

	importPath, version, ok := strings.Cut(firstPart, "@")
	if !ok || importPath == "" || version == "" {
		http.NotFound(w, r)
		return
	}

	// Look up the manifest
	data, err := mstore.Get(importPath, version)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	m, err := ParseManifest("manifest", data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Find the matching entry
	var entry *ManifestEntry
	for i := range m.Entries {
		if m.Entries[i].Output == outputPath {
			entry = &m.Entries[i]
			break
		}
	}
	if entry == nil {
		http.NotFound(w, r)
		return
	}

	// Redirect to the /go/ build endpoint
	entryVer := entry.Version
	if entryVer == "" {
		entryVer = version
	}
	redirectTo := "/go/" + entry.Source + "@" + entryVer
	http.Redirect(w, r, redirectTo, http.StatusFound)
}

// parseManifestRef parses "name@version" into (name, version, ok).
func parseManifestRef(ref string) (string, string, bool) {
	name, ver, ok := strings.Cut(ref, "@")
	if !ok || name == "" || ver == "" {
		return "", "", false
	}
	return name, ver, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
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

func handleBuildStart(w http.ResponseWriter, r *http.Request, builder *GoWasmBuilder, jobs *JobStore) {
	var req struct {
		Spec    string `json:"spec"`
		Runtime string `json:"runtime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Spec == "" {
		http.Error(w, "spec is required", http.StatusBadRequest)
		return
	}
	if req.Runtime == "" {
		req.Runtime = "go"
	}

	job := jobs.Create(req.Spec, req.Runtime)

	go runBuildJob(job, builder, jobs)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(job)
}

func handleBuildPoll(w http.ResponseWriter, jobID string, jobs *JobStore) {
	job, ok := jobs.Get(jobID)
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

func runBuildJob(job *BuildJob, builder *GoWasmBuilder, jobs *JobStore) {
	importPath, version, ok := strings.Cut(job.Spec, "@")
	if !ok || importPath == "" {
		jobs.SetError(job.ID, "spec must be pkg@version")
		return
	}
	if version == "" {
		version = "latest"
	}

	prefix := "/" + job.Runtime + "/"
	remotePath := prefix + job.Spec

	// Resolve non-canonical versions (latest, main, commit hashes)
	buildVersion := version
	resolved, err := resolveSpec(context.Background(), importPath, version)
	if err == nil && resolved.Version != version {
		canonical := prefix + importPath + "@" + resolved.Version
		_ = builder.SetAlias(remotePath, canonical)
		remotePath = canonical
		buildVersion = resolved.Version
		jobs.SetResolved(job.ID, importPath+"@"+resolved.Version)
		slog.Info("resolved version", "from", version, "to", resolved.Version, "pkg", importPath)
	} else if err != nil {
		slog.Warn("could not resolve version, building with original ref",
			"import_path", importPath, "version", version, "error", err)
		jobs.SetBuilding(job.ID)
	} else {
		jobs.SetBuilding(job.ID)
	}

	var sha string
	if job.Runtime == "tinygo" {
		sha, err = builder.TinyBuildOrWait(importPath, buildVersion, remotePath, context.Background())
	} else {
		sha, err = builder.BuildOrWait(importPath, buildVersion, remotePath, context.Background())
	}
	if err != nil {
		jobs.SetError(job.ID, err.Error())
		return
	}

	jobs.SetDone(job.ID, fmt.Sprintf("built %s -> blob/%s.wasm.gz", remotePath, sha))
}

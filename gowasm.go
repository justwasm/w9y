package w9y

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const goWasmPrefix = "/go/"

// isGoWasmPath returns true if the remotePath is a /go/... path.
func isGoWasmPath(remotePath string) bool {
	return strings.HasPrefix(remotePath, goWasmPrefix)
}

// goWasmPath contains the parsed components of a /go/ URL path.
type goWasmPath struct {
	ImportPath string // full import path (e.g. "github.com/justwasm/w9y/cmd/w9y")
	Version    string // version tag (e.g. "v0.1.0"), empty if not specified
}

// parseWasmPath parses a remote path with the given prefix into components.
//
// Accepted forms:
//
//	<prefix><import-path>@<version>  → build & serve
//	<prefix><import-path>            → no version
func parseWasmPath(remotePath, prefix string) (goWasmPath, bool) {
	trimmed := strings.TrimPrefix(remotePath, prefix)
	importPath, version, _ := strings.Cut(trimmed, "@")
	if importPath == "" {
		return goWasmPath{}, false
	}
	return goWasmPath{ImportPath: importPath, Version: version}, true
}

// parseGoWasmPath parses a /go/ remote path into its components.
func parseGoWasmPath(remotePath string) (goWasmPath, bool) {
	return parseWasmPath(remotePath, goWasmPrefix)
}

func handleGoWasm(w http.ResponseWriter, r *http.Request, builder *GoWasmBuilder, remotePath string) {
	gwp, ok := parseGoWasmPath(remotePath)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Empty version → list versions (or redirect for gist)
	if gwp.Version == "" {
		if isGistPath(gwp.ImportPath) {
			resolveGistAndRedirect(w, r, gwp.ImportPath, goWasmPrefix)
			return
		}
		listGoVersions(w, r, gwp.ImportPath)
		return
	}

	// HEAD — only check cache, never trigger a build
	if r.Method == "HEAD" {
		if e, err := builder.Get(remotePath); err == nil {
			w.Header().Set("ETag", `"`+e.Hash+`"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
		return
	}

	// Gist paths can't be resolved as Go modules — build directly
	if isGistPath(gwp.ImportPath) {
		sha, err := builder.BuildOrWait(gwp.ImportPath, gwp.Version, remotePath, r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		gzPath := blobPath(builder.DataDir(), sha, true)
		w.Header().Set("ETag", `"`+sha+`"`)
		serveFile(w, r, gzPath, true)
		return
	}

	// Best-effort resolution to canonical version
	resolved, err := resolveSpec(r.Context(), gwp.ImportPath, gwp.Version)
	if err == nil && resolved.Version != gwp.Version {
		// Create alias from non-canonical to canonical path
		canonical := goWasmPrefix + gwp.ImportPath + "@" + resolved.Version
		_ = builder.SetAlias(remotePath, canonical)
		// Redirect to canonical form
		http.Redirect(w, r, canonical, http.StatusFound)
		return
	}
	if err != nil {
		slog.Warn("could not resolve version, building with original ref",
			"import_path", gwp.ImportPath, "version", gwp.Version, "error", err)
	}

	// Concrete version — build or wait, then serve
	sha, err := builder.BuildOrWait(gwp.ImportPath, gwp.Version, remotePath, r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	gzPath := blobPath(builder.DataDir(), sha, true)
	w.Header().Set("ETag", `"`+sha+`"`)
	serveFile(w, r, gzPath, true)
}

// isGistPath returns true if the import path is a GitHub Gist.
func isGistPath(importPath string) bool {
	return strings.HasPrefix(importPath, "gist.github.com/")
}

// cloneGist clones a gist repo into dst, checks out ref (if non-empty),
// and returns the full commit hash of HEAD.
func cloneGist(ctx context.Context, importPath, ref, dst string) (string, error) {
	url := "https://" + importPath + ".git"

	slog.Info("git clone", "url", url, "dst", dst)
	cloneCmd := exec.CommandContext(ctx, "git", "clone", url, dst)
	cloneCmd.Stderr = new(bytes.Buffer)
	if err := cloneCmd.Run(); err != nil {
		stderr := cloneCmd.Stderr.(*bytes.Buffer).String()
		return "", fmt.Errorf("git clone %s: %w\n%s", url, err, stderr)
	}

	if ref != "" {
		slog.Info("git checkout", "ref", ref, "dir", dst)
		checkoutCmd := exec.CommandContext(ctx, "git", "checkout", ref)
		checkoutCmd.Dir = dst
		checkoutCmd.Stderr = new(bytes.Buffer)
		if err := checkoutCmd.Run(); err != nil {
			stderr := checkoutCmd.Stderr.(*bytes.Buffer).String()
			return "", fmt.Errorf("git checkout %s: %w\n%s", ref, err, stderr)
		}
	}

	revCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	revCmd.Dir = dst
	out, err := revCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveGistAndRedirect clones the gist, gets the latest commit hash, and
// redirects to the canonical @<commit> URL under the given prefix.
func resolveGistAndRedirect(w http.ResponseWriter, r *http.Request, importPath, prefix string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "w9y-gist-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	commitHash, err := cloneGist(ctx, importPath, "", tmpDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	canonical := prefix + importPath + "@" + commitHash
	slog.Info("redirect to canonical gist commit", "from", importPath, "to", commitHash, "prefix", prefix)
	http.Redirect(w, r, canonical, http.StatusFound)
}
func listGoVersions(w http.ResponseWriter, r *http.Request, importPath string) {
	// First, resolve the module root from the import path
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	modPath, err := moduleRoot(ctx, importPath)
	if err != nil {
		slog.Error("resolving module root", "import_path", importPath, "error", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `Specify a version with @<tag> in the URL path, e.g.:

  GET /go/%s@v0.1.0

(No versions listed — "go list -m -versions" failed for module root: %v)
`, importPath, err)
		return
	}

	versions, err := listModuleVersions(ctx, modPath)
	if err != nil {
		slog.Error("listing module versions", "module", modPath, "error", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `Specify a version with @<tag> in the URL path, e.g.:

  GET /go/%s@v0.1.0

(Module %q found, but version listing failed: %v)
`, importPath, modPath, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(map[string]any{
		"module":   modPath,
		"versions": versions,
		"message":  fmt.Sprintf("GET /go/%s@<version> to build and serve", importPath),
	})
}

type buildResult struct {
	sha string
	err error
}

type buildTask struct {
	spec       string
	runtime    string // "go" or "tinygo"
	importPath string
	version    string
	remotePath string
	jobs       []*BuildJob
	waiters    []chan buildResult
}

// GoWasmBuilder builds Go WASM binaries on demand via an internal FIFO queue
// and worker pool, deduplicating in-flight builds for the same remotePath.
type GoWasmBuilder struct {
	BlobStore
	jobs     *JobStore
	mu       sync.Mutex
	inFlight map[string]*buildTask
	queue    chan *buildTask
}

// NewGoWasmBuilder creates a new GoWasmBuilder with an internal queue and worker pool.
func NewGoWasmBuilder(store BlobStore, jobs ...*JobStore) *GoWasmBuilder {
	var js *JobStore
	if len(jobs) > 0 && jobs[0] != nil {
		js = jobs[0]
	} else {
		js = NewJobStore()
	}

	workers := 1
	if s := os.Getenv("MAX_CONCURRENT_BUILDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			workers = n
		}
	}

	b := &GoWasmBuilder{
		BlobStore: store,
		jobs:      js,
		inFlight:  make(map[string]*buildTask),
		queue:     make(chan *buildTask, 1024),
	}

	for i := 0; i < workers; i++ {
		go b.workerLoop()
	}

	return b
}

func (b *GoWasmBuilder) Jobs() *JobStore {
	return b.jobs
}

func (b *GoWasmBuilder) QueueLen() int {
	return len(b.queue)
}

func (b *GoWasmBuilder) InFlightCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.inFlight)
}

func (b *GoWasmBuilder) workerLoop() {
	for task := range b.queue {
		b.runTask(task)
	}
}

func (b *GoWasmBuilder) runTask(task *buildTask) {
	b.mu.Lock()
	if len(task.waiters) == 0 && len(task.jobs) == 0 {
		delete(b.inFlight, task.remotePath)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	// Check if already in cache before starting build
	if e, err := b.Get(task.remotePath); err == nil {
		b.finishTask(task, e.Hash, nil)
		return
	}

	// Update job statuses to building
	for _, job := range task.jobs {
		b.jobs.SetBuilding(job.ID)
	}

	// Execute build with generous timeout
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	var sha string
	var err error
	if task.runtime == "tinygo" {
		sha, err = b.doTinyBuild(ctx, task.importPath, task.version, task.remotePath)
	} else {
		sha, err = b.doBuild(ctx, task.importPath, task.version, task.remotePath)
	}

	b.finishTask(task, sha, err)
}

func (b *GoWasmBuilder) finishTask(task *buildTask, sha string, err error) {
	b.mu.Lock()
	delete(b.inFlight, task.remotePath)
	waiters := task.waiters
	b.mu.Unlock()

	for _, job := range task.jobs {
		if err != nil {
			b.jobs.SetError(job.ID, err.Error())
		} else {
			b.jobs.SetDone(job.ID, fmt.Sprintf("built %s -> blob/%s.wasm.gz", task.remotePath, sha))
		}
	}

	res := buildResult{sha: sha, err: err}
	for _, w := range waiters {
		w <- res
	}
}

func (b *GoWasmBuilder) buildOrWait(runtime, importPath, version, remotePath string, reqCtx context.Context) (string, error) {
	// Fast path: check cache before locking
	if e, err := b.Get(remotePath); err == nil {
		return e.Hash, nil
	}

	ch := make(chan buildResult, 1)

	b.mu.Lock()
	// Check cache under lock
	if e, err := b.Get(remotePath); err == nil {
		b.mu.Unlock()
		return e.Hash, nil
	}

	// If already in flight, join waiters
	if task, exists := b.inFlight[remotePath]; exists {
		task.waiters = append(task.waiters, ch)
		b.mu.Unlock()

		select {
		case res := <-ch:
			return res.sha, res.err
		case <-reqCtx.Done():
			b.removeWaiter(remotePath, ch)
			return "", reqCtx.Err()
		}
	}

	// Create new task and enqueue
	task := &buildTask{
		spec:       importPath + "@" + version,
		runtime:    runtime,
		importPath: importPath,
		version:    version,
		remotePath: remotePath,
		waiters:    []chan buildResult{ch},
	}
	b.inFlight[remotePath] = task
	b.mu.Unlock()

	b.queue <- task

	select {
	case res := <-ch:
		return res.sha, res.err
	case <-reqCtx.Done():
		b.removeWaiter(remotePath, ch)
		return "", reqCtx.Err()
	}
}

func (b *GoWasmBuilder) removeWaiter(remotePath string, ch chan buildResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	task, exists := b.inFlight[remotePath]
	if !exists {
		return
	}
	for i, w := range task.waiters {
		if w == ch {
			task.waiters = append(task.waiters[:i], task.waiters[i+1:]...)
			break
		}
	}
}

// BuildOrWait checks the cache first. If already built, returns immediately.
// Otherwise, it submits a task to the build queue and waits for the worker to complete it.
func (b *GoWasmBuilder) BuildOrWait(importPath, version, remotePath string, reqCtx context.Context) (string, error) {
	return b.buildOrWait("go", importPath, version, remotePath, reqCtx)
}

// TinyBuildOrWait is like BuildOrWait but uses TinyGo to build.
func (b *GoWasmBuilder) TinyBuildOrWait(importPath, version, remotePath string, reqCtx context.Context) (string, error) {
	return b.buildOrWait("tinygo", importPath, version, remotePath, reqCtx)
}

// EnqueueJob creates a BuildJob and queues it for asynchronous execution by the worker pool.
func (b *GoWasmBuilder) EnqueueJob(spec, runtime string) (*BuildJob, error) {
	importPath, version, ok := strings.Cut(spec, "@")
	if !ok || importPath == "" {
		return nil, errors.New("spec must be pkg@version")
	}
	if version == "" {
		version = "latest"
	}

	prefix := "/" + runtime + "/"
	remotePath := prefix + spec

	// Fast path: if already cached
	if e, err := b.Get(remotePath); err == nil {
		job := b.jobs.Create(spec, runtime)
		b.jobs.SetDone(job.ID, fmt.Sprintf("built %s -> blob/%s.wasm.gz", remotePath, e.Hash))
		return job, nil
	}

	b.mu.Lock()
	// Check cache under lock
	if e, err := b.Get(remotePath); err == nil {
		b.mu.Unlock()
		job := b.jobs.Create(spec, runtime)
		b.jobs.SetDone(job.ID, fmt.Sprintf("built %s -> blob/%s.wasm.gz", remotePath, e.Hash))
		return job, nil
	}

	job := b.jobs.Create(spec, runtime)

	// If already in flight, attach this job
	if task, exists := b.inFlight[remotePath]; exists {
		task.jobs = append(task.jobs, job)
		b.mu.Unlock()
		return job, nil
	}

	task := &buildTask{
		spec:       spec,
		runtime:    runtime,
		importPath: importPath,
		version:    version,
		remotePath: remotePath,
		jobs:       []*BuildJob{job},
	}
	b.inFlight[remotePath] = task
	b.mu.Unlock()

	b.queue <- task
	return job, nil
}

// doBuildGoWasm runs the actual Go build, stores the result, and returns the SHA.
// Uses resolveSpec to resolve the version + download the module, then builds
// from source with go build in a writable tmpdir (avoids go install restrictions).
func (b *GoWasmBuilder) doBuild(reqCtx context.Context, importPath, version, remotePath string) (string, error) {
	ctx, cancel := context.WithTimeout(reqCtx, 15*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "w9y-gowasm")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	var wasmPath string
	if isGistPath(importPath) {
		gistDir := filepath.Join(tmpDir, "gist")
		if _, err := cloneGist(ctx, importPath, version, gistDir); err != nil {
			return "", err
		}
		wasmPath, err = buildGoModule(ctx, gistDir, "", tmpDir, false)
	} else {
		// Resolve version and download module to cache
		resolved, err := resolveSpec(ctx, importPath, version)
		if err != nil {
			return "", fmt.Errorf("resolve: %w", err)
		}
		// Build from source (copy cache → tidy → go build)
		wasmPath, err = buildFromSource(ctx, resolved, tmpDir, false)
	}
	if err != nil {
		return "", err
	}

	f, err := os.Open(wasmPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Stream through gzip + sha256 simultaneously — no full []byte in memory
	gzDir := filepath.Dir(blobPath(b.DataDir(), "x", true))
	if err := os.MkdirAll(gzDir, 0o755); err != nil {
		return "", err
	}
	tmp, tmpErr := os.CreateTemp(gzDir, "*.wasm.gz")
	if tmpErr != nil {
		return "", tmpErr
	}
	tmpName := tmp.Name()
	h := sha256.New()
	gzw, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if _, err := io.Copy(io.MultiWriter(h, gzw), f); err != nil {
		gzw.Close()
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	gzw.Close()
	tmp.Close()

	sha := hex.EncodeToString(h.Sum(nil))
	gzPath := blobPath(b.DataDir(), sha, true)
	if err := os.Rename(tmpName, gzPath); err != nil {
		os.Remove(tmpName)
		return "", err
	}

	// Update mapping
	if err := b.Set(remotePath, sha); err != nil {
		return "", err
	}

	return sha, nil
}

// moduleRoot resolves the module path containing importPath.
func moduleRoot(ctx context.Context, importPath string) (string, error) {
	// If it looks like a module path (no subpackage after the module root),
	// just return it as-is.
	if isLikelyModulePath(importPath) {
		return importPath, nil
	}

	// Ask Go to resolve the module path
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Path}}", importPath)
	cmd.Env = append(os.Environ(), "GOWORK=off") // avoid workspace mode confusion
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			stderr = ": " + strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("go list -m %s: %w%s", importPath, err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

// isLikelyModulePath returns true if the path is probably a module root
// (i.e., doesn't have a subpackage). Simple heuristic: counts path segments.
// A typical module path like "github.com/user/repo" is a module root;
// "github.com/user/repo/cmd/app" is a package within a module.
func isLikelyModulePath(p string) bool {
	parts := strings.Split(p, "/")
	// Module paths are typically domain/path (at least 3 parts for github.com).
	// If there are 3 parts or fewer, it's likely a module root.
	return len(parts) <= 3
}

// listModuleVersions returns all known versions for the given module path.
func listModuleVersions(ctx context.Context, modulePath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-versions", modulePath)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m -versions: %w", err)
	}
	// Output: "<module> v0.0.1 v0.1.0 v0.2.0 ..."
	parts := strings.Fields(string(out))
	if len(parts) <= 1 {
		return []string{}, nil
	}
	return parts[1:], nil
}

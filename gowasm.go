package w9y

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	ImportPath string // full import path (e.g. "github.com/btwiuse/w9y/cmd/w9y")
	Version    string // version tag (e.g. "v0.1.0"), empty if not specified
}

// parseGoWasmPath parses a /go/ remote path into its components.
//
// Accepted forms:
//
//	/go/<import-path>@<version>  → build & serve
//	/go/<import-path>@latest    → list versions
//	/go/<import-path>           → list versions
func parseGoWasmPath(remotePath string) (goWasmPath, bool) {
	trimmed := strings.TrimPrefix(remotePath, goWasmPrefix)

	// Split on @
	atIdx := strings.IndexByte(trimmed, '@')
	var importPath, version string
	if atIdx == -1 {
		importPath = trimmed
		version = ""
	} else {
		importPath = trimmed[:atIdx]
		version = trimmed[atIdx+1:]
	}

	if importPath == "" {
		return goWasmPath{}, false
	}

	return goWasmPath{ImportPath: importPath, Version: version}, true
}

func handleGoWasm(w http.ResponseWriter, r *http.Request, builder *GoWasmBuilder, remotePath string) {
	gwp, ok := parseGoWasmPath(remotePath)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// "latest" or empty → resolve to actual latest version and redirect
	if gwp.Version == "" || gwp.Version == "latest" {
		version, err := resolvePseudoVersion(r.Context(), gwp.ImportPath, "latest")
		if err != nil {
			// Fallback: list versions if resolution fails
			listGoVersions(w, r, gwp.ImportPath)
			return
		}
		canonical := goWasmPrefix + gwp.ImportPath + "@" + version
		http.Redirect(w, r, canonical, http.StatusFound)
		return
	}

	// Resolve non-canonical references (commit hashes, branch names, etc.)
	// to their canonical pseudo-versions for cleaner mapping paths.
	if !isPseudoVersion(gwp.Version) {
		version, err := resolvePseudoVersion(r.Context(), gwp.ImportPath, gwp.Version)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if version != gwp.Version {
			canonical := goWasmPrefix + gwp.ImportPath + "@" + version
			http.Redirect(w, r, canonical, http.StatusFound)
			return
		}
	}

	// Concrete version — build or wait, then serve
	sha, err := builder.BuildOrWait(gwp.ImportPath, gwp.Version, remotePath, r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	gzPath := blobPath(builder.DataDir(), sha, true)
	serveFile(w, r, gzPath, true)
}

// isPseudoVersion reports whether s is an already-canonical Go module version
// — either a semver tag (v0.28.1) or a pseudo-version (v0.0.0-20260615105154-368618324f3e).
// Non-canonical refs (commit hashes, branch names) trigger resolution via go list.
func isPseudoVersion(s string) bool {
	if len(s) < 5 || s[0] != 'v' {
		return false
	}
	// Check it starts with v<major>.<minor>.<patch>
	dots := 0
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == '.' {
			dots++
			continue
		}
		if c < '0' || c > '9' {
			return dots >= 2
		}
	}
	return dots >= 2
}

// resolvePseudoVersion resolves a commit hash to a Go pseudo-version
// via "go list -m -json <importPath>@<commit>".
// It first resolves the module root from the import path, because
// go list -m expects module paths, not package paths.
func resolvePseudoVersion(ctx context.Context, importPath, commit string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Resolve module root first — go list -m needs a module path, not a package path
	modPath, err := moduleRoot(ctx, importPath)
	if err != nil {
		return "", fmt.Errorf("resolve module root for %s: %w", importPath, err)
	}

	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-json", modPath+"@"+commit)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			stderr = ": " + strings.TrimSpace(string(exitErr.Stderr))
		}
		return "", fmt.Errorf("resolve %s@%s%s", modPath, commit, stderr)
	}

	var info struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parse module info: %w", err)
	}
	if info.Version == "" {
		return "", fmt.Errorf("no version found for %s@%s", importPath, commit)
	}
	return info.Version, nil
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

// GoWasmBuilder builds Go WASM binaries on demand and deduplicates
// concurrent builds for the same remotePath.
type GoWasmBuilder struct {
	BlobStore
	mu     sync.Mutex
	builds map[string][]chan buildResult // remotePath → waiters
}

// NewGoWasmBuilder creates a new GoWasmBuilder.
func NewGoWasmBuilder(store BlobStore) *GoWasmBuilder {
	return &GoWasmBuilder{
		BlobStore: store,
		builds:    make(map[string][]chan buildResult),
	}
}

// BuildOrWait checks the cache first. If already built, returns immediately.
// Otherwise, it either becomes the builder or waits for the in-flight build.
func (b *GoWasmBuilder) BuildOrWait(importPath, version, remotePath string, reqCtx context.Context) (string, error) {
	// Fast path: check mapping before taking any locks
	if e, err := b.Get(remotePath); err == nil {
		return e.Hash, nil
	}

	ch := make(chan buildResult, 1)

	b.mu.Lock()
	if waiters, exists := b.builds[remotePath]; exists {
		// Someone else is building — join their waitlist
		b.builds[remotePath] = append(waiters, ch)
		b.mu.Unlock()

		res := <-ch
		return res.sha, res.err
	}

	// We are the builder
	b.builds[remotePath] = []chan buildResult{ch}
	b.mu.Unlock()

	// Do the build
	sha, err := b.doBuild(reqCtx, importPath, version, remotePath)

	// Collect all waiters and signal them
	b.mu.Lock()
	waiters := b.builds[remotePath]
	delete(b.builds, remotePath)
	b.mu.Unlock()

	res := buildResult{sha: sha, err: err}
	for _, w := range waiters {
		w <- res
	}
	return sha, err
}

// doBuildGoWasm runs the actual Go build, stores the result, and returns the SHA.
// Uses go install with an isolated temp GOPATH, since go build pkg@version
// is not supported — only go install and go get accept the @version syntax.
func (b *GoWasmBuilder) doBuild(reqCtx context.Context, importPath, version, remotePath string) (string, error) {
	ctx, cancel := context.WithTimeout(reqCtx, 5*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "w9y-gowasm")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	pkg := importPath + "@" + version

	slog.Info("building Go WASM",
		"import_path", importPath,
		"version", version,
	)

	cmd := exec.CommandContext(ctx, "go", "install",
		"-trimpath",
		"-ldflags", "-s -w",
		pkg,
	)
	cmd.Env = append(os.Environ(),
		"GOOS=js",
		"GOARCH=wasm",
		"GOPATH="+tmpDir,
		"GOWORK=off",
	)
	cmd.Stderr = new(bytes.Buffer)

	if err := cmd.Run(); err != nil {
		stderr := cmd.Stderr.(*bytes.Buffer).String()
		slog.Error("go install failed",
			"import_path", importPath,
			"version", version,
			"error", err,
			"stderr", stderr,
		)
		return "", fmt.Errorf("build failed: %s\n\n%s", err, stderr)
	}

	// Find the built wasm binary: <GOPATH>/bin/js_wasm/<basename>
	binDir := filepath.Join(tmpDir, "bin", "js_wasm")
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return "", fmt.Errorf("output not found in %s: %w", binDir, err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("output not found in %s: empty directory", binDir)
	}

	wasmPath := filepath.Join(binDir, entries[0].Name())
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

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
	importPath, version, _ := strings.Cut(trimmed, "@")

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

	// Empty version → list versions
	if gwp.Version == "" {
		listGoVersions(w, r, gwp.ImportPath)
		return
	}

	// Best-effort resolution to canonical version
	resolved, err := resolveSpec(r.Context(), gwp.ImportPath, gwp.Version)
	if err == nil && resolved.Version != gwp.Version {
		// Redirect to canonical form
		canonical := goWasmPrefix + gwp.ImportPath + "@" + resolved.Version
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
	serveFile(w, r, gzPath, true)
}

// listGoVersions handles the /go/<import-path> endpoint (no version specified).
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

	// Build directly — version is already canonical at this point
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

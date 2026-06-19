package w9y

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const tinyGoWasmPrefix = "/tinygo/"

func isTinyGoWasmPath(remotePath string) bool {
	return strings.HasPrefix(remotePath, tinyGoWasmPrefix)
}

func handleTinyGoWasm(w http.ResponseWriter, r *http.Request, builder *GoWasmBuilder, remotePath string) {
	gwp, ok := parseWasmPath(remotePath, tinyGoWasmPrefix)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if gwp.Version == "" {
		if isGistPath(gwp.ImportPath) {
			resolveGistAndRedirect(w, r, gwp.ImportPath, tinyGoWasmPrefix)
			return
		}
		http.Error(w, "specify a version with @<tag>", http.StatusBadRequest)
		return
	}

	sha, err := builder.TinyBuildOrWait(gwp.ImportPath, gwp.Version, remotePath, r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	gzPath := blobPath(builder.DataDir(), sha, true)
	serveFile(w, r, gzPath, true)
}

// TinyBuildOrWait is like BuildOrWait but uses TinyGo to build.
func (b *GoWasmBuilder) TinyBuildOrWait(importPath, version, remotePath string, reqCtx context.Context) (string, error) {
	if e, err := b.Get(remotePath); err == nil {
		return e.Hash, nil
	}

	ch := make(chan buildResult, 1)

	b.mu.Lock()
	if waiters, exists := b.builds[remotePath]; exists {
		b.builds[remotePath] = append(waiters, ch)
		b.mu.Unlock()
		res := <-ch
		return res.sha, res.err
	}

	b.builds[remotePath] = []chan buildResult{ch}
	b.mu.Unlock()

	sha, err := b.doTinyBuild(reqCtx, importPath, version, remotePath)

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

func (b *GoWasmBuilder) doTinyBuild(reqCtx context.Context, importPath, version, remotePath string) (string, error) {
	ctx, cancel := context.WithTimeout(reqCtx, 15*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "w9y-tinygowasm")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	var buildDir string
	var modRoot string
	if isGistPath(importPath) {
		gistDir := filepath.Join(tmpDir, "gist")
		if _, err := cloneGist(ctx, importPath, version, gistDir); err != nil {
			return "", err
		}
		buildDir = gistDir
		modRoot = gistDir

		// Init go.mod if missing
		if _, err := os.Stat(filepath.Join(gistDir, "go.mod")); os.IsNotExist(err) {
			slog.Info("go mod init", "dir", gistDir)
			initCmd := exec.CommandContext(ctx, "go", "mod", "init", importPath)
			initCmd.Dir = gistDir
			initCmd.Env = append(os.Environ(), "GOWORK=off")
			initCmd.Stderr = new(bytes.Buffer)
			if err := initCmd.Run(); err != nil {
				stderr := initCmd.Stderr.(*bytes.Buffer).String()
				if stderr != "" {
					fmt.Fprint(os.Stderr, stderr)
				}
				return "", fmt.Errorf("go mod init: %w", err)
			}

			slog.Info("go mod edit -go=1.26", "dir", gistDir)
			verCmd := exec.CommandContext(ctx, "go", "mod", "edit", "-go=1.26")
			verCmd.Dir = gistDir
			verCmd.Env = append(os.Environ(), "GOWORK=off")
			if err := verCmd.Run(); err != nil {
				return "", fmt.Errorf("go mod edit -go=1.26: %w", err)
			}
		}
	} else {
		resolved, err := resolveSpec(ctx, importPath, version)
		if err != nil {
			return "", fmt.Errorf("resolve: %w", err)
		}

		modDir := filepath.Join(tmpDir, "mod")
		if err := os.CopyFS(modDir, os.DirFS(resolved.Dir)); err != nil {
			return "", fmt.Errorf("copy module source: %w", err)
		}

		buildDir = modDir
		modRoot = modDir
		if resolved.Path != "" {
			buildDir = filepath.Join(modDir, resolved.Path)
		}
	}

	// Apply replace directives for WASM compatibility (clipboard, bubbletea)
	replaceCtx, replaceCancel := context.WithTimeout(ctx, 1*time.Minute)
	defer replaceCancel()

	slog.Info("go mod edit -replace", "dir", modRoot)
	replaceCmd := exec.CommandContext(replaceCtx, "go", "mod", "edit",
		"-replace", "github.com/atotto/clipboard=github.com/justwasm/clipboard@v0.1.6",
		"-replace", "charm.land/bubbletea/v2=github.com/bubbletui/bubbletea/v2@v2.0.10")
	replaceCmd.Dir = modRoot
	replaceCmd.Env = append(os.Environ(), "GOWORK=off")
	replaceCmd.Stderr = new(bytes.Buffer)
	if err := replaceCmd.Run(); err != nil {
		stderr := replaceCmd.Stderr.(*bytes.Buffer).String()
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		return "", fmt.Errorf("go mod edit -replace: %w", err)
	}

	// Run go mod tidy to resolve dependencies
	tidyCtx, tidyCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer tidyCancel()

	slog.Info("go mod tidy", "dir", buildDir)
	tidyCmd := exec.CommandContext(tidyCtx, "go", "mod", "tidy")
	tidyCmd.Dir = buildDir
	tidyCmd.Env = append(os.Environ(), "GOWORK=off")
	tidyCmd.Stderr = new(bytes.Buffer)
	if err := tidyCmd.Run(); err != nil {
		stderr := tidyCmd.Stderr.(*bytes.Buffer).String()
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		return "", fmt.Errorf("go mod tidy: %w", err)
	}

	// Build with TinyGo
	wasmPath := filepath.Join(tmpDir, "output.wasm")
	args := []string{"build", "-o", wasmPath, "-target=wasm", "."}
	slog.Info("tinygo "+strings.Join(args, " "), "dir", buildDir)

	tinyCtx, tinyCancel := context.WithTimeout(ctx, 15*time.Minute)
	defer tinyCancel()

	buildCmd := exec.CommandContext(tinyCtx, "tinygo", args...)
	buildCmd.Dir = buildDir
	buildCmd.Stderr = new(bytes.Buffer)
	if err := buildCmd.Run(); err != nil {
		stderr := buildCmd.Stderr.(*bytes.Buffer).String()
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		return "", fmt.Errorf("tinygo build: %w", err)
	}

	// Store result — same pattern as doBuild
	f, err := os.Open(wasmPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

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

	if err := b.Set(remotePath, sha); err != nil {
		return "", err
	}

	return sha, nil
}

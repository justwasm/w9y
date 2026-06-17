package w9y

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newBuildCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build <pkg>@<version>",
		Short: "Build a Go WASM binary locally",
		Long: `Build a Go WASM binary from a Go package path and version, then print the
output path. Useful for local debugging of build issues.

Examples:
  w9y build github.com/btwiuse/w9y/cmd/w9y@latest
  w9y build github.com/btwiuse/w9y/cmd/w9y@v0.0.1
  w9y build github.com/btwiuse/w9y/cmd/w9y@b45ecc4
`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runBuild(args[0])
		},
	}
	return cmd
}

// resolvedSpec holds the components of a resolved build spec.
type resolvedSpec struct {
	Pkg     string // module import path (e.g. "github.com/btwiuse/w9y")
	Path    string // subpackage path within the module (e.g. "cmd/w9y")
	Version string // resolved canonical version (e.g. "v0.0.0-20260616064018-2314db5ec7ed")
	Dir     string // cached module directory from go mod download
}

// resolveSpec resolves a non-canonical version reference (commit hash, branch,
// "latest") to a canonical Go module version and downloads the module to the
// Go module cache in a single step. It tries progressively shorter import
// paths via go mod download -json, following go get's module resolution logic.
func resolveSpec(ctx context.Context, importPath, version string) (resolvedSpec, error) {
	if version == "" {
		version = "latest"
	}

	// Try progressively shorter import paths via go mod download -json.
	// This resolves the version and downloads the module in one call.
	parts := strings.Split(importPath, "/")
	for i := len(parts); i >= 1; i-- {
		modPath := strings.Join(parts[:i], "/")
		spec := modPath + "@" + version

		cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", spec)
		cmd.Env = append(os.Environ(), "GOWORK=off")
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		slog.Debug("go mod download -json " + spec)
		out, err := cmd.Output()
		if err != nil {
			if stderr := stderrBuf.String(); stderr != "" {
				fmt.Fprintf(os.Stderr, "go mod download -json %s:\n%s", spec, stderr)
			}
			continue
		}

		var info struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
			Dir     string `json:"Dir"`
		}
		if err := json.Unmarshal(out, &info); err != nil {
			continue
		}
		if info.Version == "" || info.Dir == "" {
			continue
		}

		// Subpackage path is whatever remains after stripping the module root
		sub := strings.TrimPrefix(importPath, modPath+"/")
		if sub == importPath {
			sub = ""
		}

		slog.Info("resolved version",
			"from", version, "to", info.Version,
			"pkg", info.Path, "path", sub)

		return resolvedSpec{
			Pkg:     info.Path,
			Path:    sub,
			Version: info.Version,
			Dir:     info.Dir,
		}, nil
	}

	return resolvedSpec{}, fmt.Errorf("could not resolve %s@%s", importPath, version)
}

func buildGoModule(ctx context.Context, modDir string, subPkg string, tmpDir string) (string, error) {
	// Initialize go.mod if missing (e.g. for gists without one)
	if _, err := os.Stat(filepath.Join(modDir, "go.mod")); os.IsNotExist(err) {
		slog.Info("go mod init gist", "dir", modDir)
		initCmd := exec.CommandContext(ctx, "go", "mod", "init", "gist")
		initCmd.Dir = modDir
		initCmd.Env = append(os.Environ(), "GOWORK=off")
		initCmd.Stderr = new(bytes.Buffer)
		if err := initCmd.Run(); err != nil {
			stderr := initCmd.Stderr.(*bytes.Buffer).String()
			if stderr != "" {
				fmt.Fprint(os.Stderr, stderr)
			}
			return "", fmt.Errorf("go mod init: %w", err)
		}

		// Set Go version in go.mod
		slog.Info("go mod edit -go=1.26", "dir", modDir)
		verCmd := exec.CommandContext(ctx, "go", "mod", "edit", "-go=1.26")
		verCmd.Dir = modDir
		verCmd.Env = append(os.Environ(), "GOWORK=off")
		if err := verCmd.Run(); err != nil {
			return "", fmt.Errorf("go mod edit -go=1.26: %w", err)
		}
	}

	// Run go mod edit -replace before tidy to swap clipboard fork
	replaceCtx, replaceCancel := context.WithTimeout(ctx, 1*time.Minute)
	defer replaceCancel()

	slog.Info("go mod edit -replace", "dir", modDir)
	replaceCmd := exec.CommandContext(replaceCtx, "go", "mod", "edit",
		"-replace", "github.com/atotto/clipboard=github.com/justwasm/clipboard@v0.1.6",
		"-replace", "charm.land/bubbletea/v2=github.com/bubbletui/bubbletea/v2@v2.0.8")
	replaceCmd.Dir = modDir
	replaceCmd.Env = append(os.Environ(), "GOWORK=off")
	replaceCmd.Stderr = new(bytes.Buffer)
	if err := replaceCmd.Run(); err != nil {
		stderr := replaceCmd.Stderr.(*bytes.Buffer).String()
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		return "", fmt.Errorf("go mod edit -replace: %w", err)
	}

	// Run go mod tidy to resolve dependencies and generate go.sum
	tidyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	slog.Info("go mod tidy", "dir", modDir)
	tidyCmd := exec.CommandContext(tidyCtx, "go", "mod", "tidy")
	tidyCmd.Dir = modDir
	tidyCmd.Env = append(os.Environ(), "GOWORK=off")
	tidyCmd.Stderr = new(bytes.Buffer)
	if err := tidyCmd.Run(); err != nil {
		stderr := tidyCmd.Stderr.(*bytes.Buffer).String()
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		return "", fmt.Errorf("go mod tidy: %w", err)
	}

	// Build from the package directory
	buildDir := modDir
	if subPkg != "" {
		buildDir = filepath.Join(modDir, subPkg)
	}

	wasmPath := filepath.Join(tmpDir, "output.wasm")
	args := []string{"build", "-trimpath", "-ldflags", "-s -w", "-o", wasmPath, "."}
	slog.Info("go " + strings.Join(args, " "), "dir", buildDir)
	buildCmd := exec.CommandContext(ctx, "go", args...)
	buildCmd.Dir = buildDir
	buildCmd.Env = append(os.Environ(),
		"GOOS=js",
		"GOARCH=wasm",
		"GOWORK=off",
		"CGO_ENABLED=0",
	)
	buildCmd.Stderr = new(bytes.Buffer)
	if err := buildCmd.Run(); err != nil {
		stderr := buildCmd.Stderr.(*bytes.Buffer).String()
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		return "", fmt.Errorf("build failed: %w", err)
	}

	return wasmPath, nil
}

// buildFromSource copies the already-downloaded module from the Go module cache
// to a writable tmpdir, runs go mod tidy, then builds with go build GOOS=js
// GOARCH=wasm. This approach treats the module as the main module, so replace
// directives in go.mod are honored (unlike go install which rejects them).
func buildFromSource(ctx context.Context, spec resolvedSpec, tmpDir string) (string, error) {
	// Copy module source from cache to tmpdir (cache is often read-only)
	modDir := filepath.Join(tmpDir, "mod")
	if err := os.CopyFS(modDir, os.DirFS(spec.Dir)); err != nil {
		return "", fmt.Errorf("copy module source: %w", err)
	}

	return buildGoModule(ctx, modDir, spec.Path, tmpDir)
}

func runBuild(spec string) error {
	importPath, version, ok := strings.Cut(spec, "@")
	if !ok || importPath == "" {
		return fmt.Errorf("usage: w9y build <pkg>@<version> (got %q)", spec)
	}
	if version == "" {
		version = "latest"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "w9y-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	var wasmPath string
	var pkgBase string

	if isGistPath(importPath) {
		gistDir := filepath.Join(tmpDir, "gist")
		if _, err := cloneGist(ctx, importPath, version, gistDir); err != nil {
			return err
		}
		parts := strings.Split(strings.TrimRight(importPath, "/"), "/")
		pkgBase = parts[len(parts)-1]
		wasmPath, err = buildGoModule(ctx, gistDir, "", tmpDir)
	} else {
		// Resolve version and download module in one step
		resolved, err := resolveSpec(ctx, importPath, version)
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}
		pkgBase = filepath.Base(resolved.Path)
		if resolved.Path == "" {
			pkgBase = filepath.Base(resolved.Pkg)
		}
		wasmPath, err = buildFromSource(ctx, resolved, tmpDir)
	}
	if err != nil {
		return err
	}

	// Copy to CWD so it survives tmp dir cleanup
	dst := filepath.Join(".", pkgBase+".wasm")
	data, err := os.ReadFile(wasmPath)
	if err != nil {
		return fmt.Errorf("read wasm output: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}

	abs, _ := filepath.Abs(dst)
	fmt.Println(abs)
	return nil
}

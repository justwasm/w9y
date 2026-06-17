package w9y

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newTinyBuildCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tinybuild <pkg>@<version>",
		Short: "Build a Go WASM binary using TinyGo",
		Long: `Build a Go WASM binary from a Go package path and version using TinyGo,
then print the output path. Useful for TinyGo-specific WASM builds.

Examples:
  w9y tinybuild github.com/user/repo@latest
  w9y tinybuild github.com/user/repo/cmd/app@v0.1.0
  w9y tinybuild github.com/user/repo@commit-hash
`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runTinyBuild(args[0])
		},
	}
	return cmd
}

func runTinyBuild(spec string) error {
	importPath, version, ok := strings.Cut(spec, "@")
	if !ok || importPath == "" {
		return fmt.Errorf("usage: w9y tinybuild <pkg>@<version> (got %q)", spec)
	}
	if version == "" {
		version = "latest"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "w9y-tinybuild-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	var buildDir string
	var pkgBase string

	if isGistPath(importPath) {
		gistDir := filepath.Join(tmpDir, "gist")
		if _, err := cloneGist(ctx, importPath, version, gistDir); err != nil {
			return err
		}
		buildDir = gistDir
		// Derive binary name from the gist id (last path segment)
		parts := strings.Split(strings.TrimRight(importPath, "/"), "/")
		pkgBase = parts[len(parts)-1]

		// Initialize go.mod if missing
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
				return fmt.Errorf("go mod init: %w", err)
			}

			slog.Info("go mod edit -go=1.26", "dir", gistDir)
			verCmd := exec.CommandContext(ctx, "go", "mod", "edit", "-go=1.26")
			verCmd.Dir = gistDir
			verCmd.Env = append(os.Environ(), "GOWORK=off")
			if err := verCmd.Run(); err != nil {
				return fmt.Errorf("go mod edit -go=1.26: %w", err)
			}
		}
	} else {
		// Resolve version and download module in one step
		resolved, err := resolveSpec(ctx, importPath, version)
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}

		// Copy module source from cache to tmpdir
		modDir := filepath.Join(tmpDir, "mod")
		if err := os.CopyFS(modDir, os.DirFS(resolved.Dir)); err != nil {
			return fmt.Errorf("copy module source: %w", err)
		}

		buildDir = modDir
		if resolved.Path != "" {
			buildDir = filepath.Join(modDir, resolved.Path)
			pkgBase = filepath.Base(resolved.Path)
		} else {
			pkgBase = filepath.Base(resolved.Pkg)
		}

		// Initialize go.mod if missing
		if _, err := os.Stat(filepath.Join(modDir, "go.mod")); os.IsNotExist(err) {
			slog.Info("go mod init", "dir", modDir)
			initCmd := exec.CommandContext(ctx, "go", "mod", "init", importPath)
			initCmd.Dir = modDir
			initCmd.Env = append(os.Environ(), "GOWORK=off")
			initCmd.Stderr = new(bytes.Buffer)
			if err := initCmd.Run(); err != nil {
				stderr := initCmd.Stderr.(*bytes.Buffer).String()
				if stderr != "" {
					fmt.Fprint(os.Stderr, stderr)
				}
				return fmt.Errorf("go mod init: %w", err)
			}

			slog.Info("go mod edit -go=1.26", "dir", modDir)
			verCmd := exec.CommandContext(ctx, "go", "mod", "edit", "-go=1.26")
			verCmd.Dir = modDir
			verCmd.Env = append(os.Environ(), "GOWORK=off")
			if err := verCmd.Run(); err != nil {
				return fmt.Errorf("go mod edit -go=1.26: %w", err)
			}
		}
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
		return fmt.Errorf("go mod tidy: %w", err)
	}

	// Build with tinygo
	wasmPath := filepath.Join(tmpDir, "output.wasm")
	args := []string{"build", "-o", wasmPath, "-target=wasm", "."}
	slog.Info("tinygo " + strings.Join(args, " "), "dir", buildDir)

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
		return fmt.Errorf("tinygo build failed: %w", err)
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

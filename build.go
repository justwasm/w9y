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
	"golang.org/x/mod/semver"
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

// resolvedSpec holds the three components of a resolved build spec.
type resolvedSpec struct {
	Pkg     string // module import path (e.g. "github.com/btwiuse/w9y")
	Path    string // subpackage path within the module (e.g. "cmd/w9y")
	Version string // resolved canonical version (e.g. "v0.0.0-20260616064018-2314db5ec7ed")
}

// importPath returns the full package import path (pkg + "/" + path).
func (r resolvedSpec) importPath() string {
	if r.Path == "" {
		return r.Pkg
	}
	return r.Pkg + "/" + r.Path
}

// resolveSpec resolves a non-canonical version reference (commit hash, branch
// name, "latest") to a canonical Go module version. It tries progressively
// shorter import paths, following go get's module resolution logic.
//
// If version is already a valid semver, path is empty and pkg equals the input.
func resolveSpec(ctx context.Context, pkg, version string) (resolvedSpec, error) {
	if version == "" {
		version = "latest"
	}

	// Already canonical — no resolution needed
	if semver.IsValid(version) {
		return resolvedSpec{Pkg: pkg, Path: "", Version: version}, nil
	}

	// Try progressively shorter import paths
	parts := strings.Split(pkg, "/")
	for i := len(parts); i >= 1; i-- {
		modPath := strings.Join(parts[:i], "/")
		spec := modPath + "@" + version
		cmd := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Version}}", spec)
		cmd.Env = append(os.Environ(), "GOWORK=off")
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		out, err := cmd.Output()
		if err != nil {
			if stderr := stderrBuf.String(); stderr != "" {
				fmt.Fprintf(os.Stderr, "go list -m -f {{.Version}} %s:\n%s", spec, stderr)
			}
			continue
		}
		ver := strings.TrimSpace(string(out))
		if ver != "" && semver.IsValid(ver) {
			// The subpackage path is whatever remains after stripping the module root
			sub := strings.TrimPrefix(pkg, modPath+"/")
			if sub == pkg {
				sub = "" // pkg == modPath (no subpackage)
			}
			return resolvedSpec{Pkg: modPath, Path: sub, Version: ver}, nil
		}
	}

	return resolvedSpec{}, fmt.Errorf("could not resolve %s@%s", pkg, version)
}

// buildFromSource downloads the module via go mod download, copies it to a
// writable tmpdir, runs go mod tidy, and builds with go build. This approach
// avoids go install's restriction on replace directives in dependency go.mod
// files, because the module is treated as the main module during build.
func buildFromSource(ctx context.Context, spec resolvedSpec, tmpDir string) (string, error) {
	modSpec := spec.Pkg + "@" + spec.Version

	// 1. Download module to Go's module cache
	slog.Info("downloading module", "spec", modSpec)
	downloadCmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", modSpec)
	downloadCmd.Env = append(os.Environ(), "GOWORK=off")
	downloadCmd.Stderr = new(bytes.Buffer)
	out, err := downloadCmd.Output()
	if err != nil {
		stderr := downloadCmd.Stderr.(*bytes.Buffer).String()
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		return "", fmt.Errorf("go mod download: %w", err)
	}

	var info struct {
		Dir string `json:"Dir"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("parse download info: %w", err)
	}
	if info.Dir == "" {
		return "", fmt.Errorf("module %s: empty Dir from go mod download", modSpec)
	}

	// 2. Copy module source from cache to tmpdir (writable, so tidy/build work)
	modDir := filepath.Join(tmpDir, "mod")
	if err := copyDir(info.Dir, modDir); err != nil {
		return "", fmt.Errorf("copy module source: %w", err)
	}

	// 3. Run go mod tidy to resolve dependencies and generate go.sum
	tidyCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	slog.Info("running go mod tidy")
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

	// 4. Build with GOOS=js GOARCH=wasm
	buildDir := modDir
	if spec.Path != "" {
		buildDir = filepath.Join(modDir, spec.Path)
	}

	wasmPath := filepath.Join(tmpDir, "output.wasm")

	slog.Info("building Go WASM", "pkg", spec.Pkg, "path", spec.Path, "version", spec.Version)
	buildCmd := exec.CommandContext(ctx, "go", "build",
		"-trimpath",
		"-ldflags", "-s -w",
		"-o", wasmPath,
		".",
	)
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

// copyDir recursively copies a directory tree from src to dst.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
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

	// Best-effort version resolution
	resolved, err := resolveSpec(ctx, importPath, version)
	if err == nil {
		if resolved.Version != version {
			slog.Info("resolved version",
				"pkg", resolved.Pkg, "path", resolved.Path,
				"from", version, "to", resolved.Version)
		}
	} else {
		slog.Warn("could not resolve version, building with original ref", "spec", spec, "error", err)
		resolved = resolvedSpec{Pkg: importPath, Path: ""}
	}

	// Build from proxy source
	tmpDir, err := os.MkdirTemp("", "w9y-build-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	wasmPath, err := buildFromSource(ctx, resolved, tmpDir)
	if err != nil {
		return err
	}

	// Copy to CWD so it survives tmp dir cleanup
	dst := filepath.Join(".", filepath.Base(resolved.Path)+".wasm")
	if resolved.Path == "" {
		dst = filepath.Join(".", filepath.Base(resolved.Pkg)+".wasm")
	}
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

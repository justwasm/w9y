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
	Pkg     string // original package import path (e.g. "github.com/btwiuse/w9y/cmd/w9y")
	ModPath string // resolved module path (e.g. "github.com/btwiuse/w9y")
	Version string // resolved canonical version (e.g. "v0.0.0-20260616064018-2314db5ec7ed")
}

// resolveSpec resolves a non-canonical version reference (commit hash, branch
// name, "latest") to a canonical Go module version. It tries progressively
// shorter import paths, following go get's module resolution logic.
//
// If version is already a valid semver, pkg and modPath both equal the input.
func resolveSpec(ctx context.Context, pkg, version string) (resolvedSpec, error) {
	if version == "" {
		version = "latest"
	}

	// Already canonical — no resolution needed
	if semver.IsValid(version) {
		return resolvedSpec{Pkg: pkg, ModPath: pkg, Version: version}, nil
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
			return resolvedSpec{Pkg: pkg, ModPath: modPath, Version: ver}, nil
		}
	}

	return resolvedSpec{}, fmt.Errorf("could not resolve %s@%s", pkg, version)
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
				"pkg", importPath, "module", resolved.ModPath,
				"from", version, "to", resolved.Version)
		}
		version = resolved.Version
	} else {
		slog.Warn("could not resolve version, building with original ref", "spec", spec, "error", err)
	}

	// Build
	tmpDir, err := os.MkdirTemp("", "w9y-build")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	pkg := importPath + "@" + version

	slog.Info("building Go WASM", "import_path", importPath, "version", version)

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
		slog.Error("build failed",
			"import_path", importPath,
			"version", version,
			"error", err,
		)
		// Print full build output for debugging
		if stderr != "" {
			fmt.Fprintln(os.Stderr, stderr)
		}
		return fmt.Errorf("build failed: %w", err)
	}

	// Find output
	binDir := filepath.Join(tmpDir, "bin", "js_wasm")
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return fmt.Errorf("output not found in %s: %w", binDir, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("output not found in %s: empty directory", binDir)
	}

	// Copy to CWD so it survives tmp dir cleanup
	src := filepath.Join(binDir, entries[0].Name())
	dst := filepath.Join(".", entries[0].Name())
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read built wasm: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}

	abs, _ := filepath.Abs(dst)
	fmt.Println(abs)
	return nil
}

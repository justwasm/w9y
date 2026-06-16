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
	if version == "latest" {
		v, err := resolvePseudoVersion(ctx, importPath, "latest")
		if err == nil {
			version = v
		} else {
			slog.Warn("could not resolve latest, building @latest directly", "error", err)
		}
	} else if !isPseudoVersion(version) {
		v, err := resolvePseudoVersion(ctx, importPath, version)
		if err == nil && v != version {
			slog.Info("resolved to canonical version", "from", version, "to", v)
			version = v
		} else if err != nil {
			slog.Warn("could not resolve canonical version, building with original ref", "original", version, "error", err)
		}
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

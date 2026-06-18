package w9y

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newResolveCommand() *cobra.Command {
	var outputJSON bool

	cmd := &cobra.Command{
		Use:   "resolve <pkg>@<version>",
		Short: "Resolve a Go module version reference to its canonical form",
		Long: `Resolve a non-canonical version reference (commit hash, branch, "latest")
to a canonical Go module version.

Examples:
  w9y resolve github.com/gearshell/logo/cmd/logo@latest
  w9y resolve --json github.com/gearshell/logo/cmd/logo@latest
  w9y resolve github.com/btwiuse/w9y/cmd/w9y@b45ecc4
  w9y resolve github.com/btwiuse/w9y@v0.0.1
`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runResolve(args[0], outputJSON)
		},
	}
	cmd.Flags().BoolVar(&outputJSON, "json", false, "output as JSON")
	return cmd
}

func runResolve(spec string, outputJSON bool) error {
	importPath, version, ok := strings.Cut(spec, "@")
	if !ok || importPath == "" {
		return fmt.Errorf("usage: w9y resolve <pkg>@<version> (got %q)", spec)
	}
	if version == "" {
		version = "latest"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	resolved, err := resolveSpec(ctx, importPath, version)
	if err != nil {
		return err
	}

	if outputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(resolved)
	}

	fmt.Printf("%s@%s\n", resolved.Pkg, resolved.Version)
	return nil
}

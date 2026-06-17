package w9y

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
)

func newSemverCommand() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:   "semver <string>",
		Short: "Check if a string is a valid semantic version",
		Long: `Validate a semantic version string. Exits with 0 if valid, 1 if not.

Examples:
  w9y semver v1.2.3        → valid
  w9y semver v1.2.3-beta.1 → valid
  w9y semver 1.2.3         → invalid (missing v prefix)
  w9y semver foo           → invalid`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if verbose {
				slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
			}

			v := args[0]
			ok := semver.IsValid(v)

			if ok {
				slog.Info("valid semantic version",
					"version", v,
					"canonical", semver.Canonical(v),
					"major", semver.Major(v),
					"majorMinor", semver.MajorMinor(v),
				)
				fmt.Println("valid")
				return nil
			}

			// Diagnose why it's invalid
			dash := ""
			if len(v) > 0 && v[0] != 'v' {
				dash = "missing leading \"v\" prefix"
			}
			slog.Warn("invalid semantic version",
				"version", v,
				"reason", dash,
			)
			fmt.Println("invalid")
			os.Exit(1)
			return nil
		},
	}

	cmd.Flags().BoolVar(&verbose, "verbose", false, "show detailed version info")

	return cmd
}

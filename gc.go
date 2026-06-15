package w9y

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

func newGCCommand() *cobra.Command {
	var clean bool
	cmd := &cobra.Command{
		Use:   "gc [-data-dir data] [-clean]",
		Short: "Find and remove unreferenced blobs",
		Long: `Find unreferenced blobs in the data directory.

By default, only lists orphans (dry-run). Use -clean to delete them.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return gcData(dataDir, clean)
		},
	}
	cmd.Flags().BoolVar(&clean, "clean", false, "delete orphaned blobs instead of dry-run")
	return cmd
}

func gcData(dataDir string, clean bool) error {
	store := NewBlobStore(dataDir)
	entries, err := store.List()
	if err != nil {
		return err
	}

	referenced := make(map[string]bool, len(entries))
	for _, e := range entries {
		referenced[e.Hash] = true
	}

	files, err := listBlobFiles(dataDir)
	if err != nil {
		return err
	}

	for _, f := range files {
		if referenced[f.SHA] {
			continue
		}
		if clean {
			if err := os.Remove(f.Path); err != nil {
				return fmt.Errorf("remove %s: %v", f.Name, err)
			}
			slog.Info("removed orphan blob", "name", f.Name)
		} else {
			slog.Info("orphan blob", "name", f.Name)
		}
	}

	return nil
}

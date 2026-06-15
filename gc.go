package w9y

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newGCCommand() *cobra.Command {
	var (
		dataDir string
		clean   bool
	)
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
	cmd.Flags().StringVar(&dataDir, "data-dir", getenv("DATA_DIR", defaultDataDir), "data directory")
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

	blobDir := filepath.Join(dataDir, "blob")
	dirEntries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, de := range dirEntries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		sha, ok := strings.CutSuffix(name, ".wasm.gz")
		if !ok {
			continue
		}
		if referenced[sha] {
			continue
		}
		path := filepath.Join(blobDir, name)
		if clean {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove %s: %v", name, err)
			}
			slog.Info("removed orphan blob", "name", name)
		} else {
			slog.Info("orphan blob", "name", name)
		}
	}

	return nil
}

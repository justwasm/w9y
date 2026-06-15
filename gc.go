package w9y

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func gc(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	clean := fs.Bool("clean", false, "delete orphaned blobs instead of dry-run")
	dataDir := fs.String("data-dir", getenv("DATA_DIR", defaultDataDir), "data directory")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: w9y gc [-data-dir data] [-clean]

Find unreferenced blobs in the data directory.

flags:
  -data-dir  data directory (default data)
  -clean     delete orphaned blobs instead of dry-run`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	return gcData(*dataDir, *clean)
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

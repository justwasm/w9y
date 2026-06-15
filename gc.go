package w9y

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func gc(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	clean := fs.Bool("clean", false, "delete orphaned blobs instead of dry-run")
	dataDir := fs.String("data-dir", getenv("DATA_DIR", defaultDataDir), "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return gcData(*dataDir, *clean)
}

func gcData(dataDir string, clean bool) error {
	m, err := loadMapping(dataDir)
	if err != nil {
		return err
	}

	referenced := make(map[string]bool, len(m.Entries))
	for _, e := range m.Entries {
		referenced[e.SHA] = true
	}

	blobDir := filepath.Join(dataDir, "blob")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
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
			fmt.Println("removed", name)
		} else {
			fmt.Println(name)
		}
	}

	return nil
}

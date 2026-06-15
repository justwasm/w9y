package w9y

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "backup destination directory (default data)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: w9y backup [-data-dir data] [dest-dir]

Backup all entries and blobs from remote server to a local directory.

flags:
  -data-dir  destination directory (default data)`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	host := defaultHost
	destDir := fs.Arg(0)
	if destDir == "" {
		destDir = *dataDir
	}
	if destDir == "" {
		destDir = "data"
	}
	return backupRemote(http.DefaultClient, host, destDir)
}

func backupRemote(client *http.Client, host, destDir string) error {
	u, err := url.Parse(host)
	if err != nil {
		return err
	}

	resp, err := client.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var items []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Time   string `json:"time"`
		Size   string `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return err
	}

	// Merge: load existing local mapping, overlay remote entries
	m, err := loadMapping(destDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "fetched %d entries from remote\n", len(items))
	for _, item := range items {
		t, parseErr := time.Parse(time.RFC3339Nano, item.Time)
		if parseErr != nil {
			return fmt.Errorf("parse time %q: %v", item.Time, parseErr)
		}
		m.Entries[item.Path] = entry{SHA: item.SHA256, Time: t.UnixMilli()}
	}
	if err := saveMapping(destDir, m); err != nil {
		return err
	}

	blobClient := client
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := tr.Clone()
		clone.DisableCompression = true
		blobClient = &http.Client{Transport: clone}
	}

	seen := make(map[string]bool, len(items))
	var downloaded, skipped int
	for _, item := range items {
		if seen[item.SHA256] {
			continue
		}
		seen[item.SHA256] = true

		gzPath := blobPath(destDir, item.SHA256, true)
		if _, err := os.Stat(gzPath); err == nil {
			skipped++
			continue // already exists locally
		}

		blobU, _ := url.Parse(host)
		blobU.Path, _ = url.JoinPath(blobU.Path, blobRemotePath(item.SHA256, true))
		blobResp, err := blobClient.Get(blobU.String())
		if err != nil {
			return fmt.Errorf("download blob %s: %v", item.SHA256, err)
		}
		gzData, readErr := io.ReadAll(blobResp.Body)
		blobResp.Body.Close()
		if blobResp.StatusCode != http.StatusOK {
			return fmt.Errorf("download blob %s: %s", item.SHA256, blobResp.Status)
		}
		if readErr != nil {
			return fmt.Errorf("read blob %s: %v", item.SHA256, readErr)
		}

		if err := os.MkdirAll(filepath.Dir(gzPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(gzPath, gzData, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "downloaded blob/%s.wasm.gz (%.2f MB)\n", item.SHA256, float64(len(gzData))/(1024*1024))
		downloaded++
	}

	if downloaded > 0 || skipped > 0 {
		fmt.Fprintf(os.Stderr, "backup complete: %d downloaded, %d already present\n", downloaded, skipped)
	}

	return nil
}

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
	if err := fs.Parse(args); err != nil {
		return err
	}
	host := defaultHost
	destDir := fs.Arg(0)
	if destDir == "" {
		destDir = "backup-" + time.Now().Format("20060102-150405")
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
		Path string `json:"path"`
		SHA  string `json:"sha"`
		Time int64  `json:"time"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return err
	}

	// Merge: load existing local mapping, overlay remote entries
	m, err := loadMapping(destDir)
	if err != nil {
		return err
	}
	for _, item := range items {
		m.Entries[item.Path] = entry{SHA: item.SHA, Time: item.Time}
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
	for _, item := range items {
		if seen[item.SHA] {
			continue
		}
		seen[item.SHA] = true

		gzPath := blobPath(destDir, item.SHA, true)
		if _, err := os.Stat(gzPath); err == nil {
			continue // already exists locally
		}

		blobU, _ := url.Parse(host)
		blobU.Path, _ = url.JoinPath(blobU.Path, blobRemotePath(item.SHA, true))
		blobResp, err := blobClient.Get(blobU.String())
		if err != nil {
			return fmt.Errorf("download blob %s: %v", item.SHA, err)
		}
		gzData, readErr := io.ReadAll(blobResp.Body)
		blobResp.Body.Close()
		if blobResp.StatusCode != http.StatusOK {
			return fmt.Errorf("download blob %s: %s", item.SHA, blobResp.Status)
		}
		if readErr != nil {
			return fmt.Errorf("read blob %s: %v", item.SHA, readErr)
		}

		if err := os.MkdirAll(filepath.Dir(gzPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(gzPath, gzData, 0o644); err != nil {
			return err
		}
	}

	return nil
}

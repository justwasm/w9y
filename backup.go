package w9y

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dataDir := fs.String("data-dir", getenv("DATA_DIR", "data"), "data directory")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: w9y backup [-data-dir data]

Backup all entries and blobs from remote server to local data directory.

flags:
  -data-dir  data directory (default data)`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("backup takes no positional arguments")
	}
	host := defaultHost
	return backupRemote(http.DefaultClient, host, *dataDir)
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
		Hash string `json:"hash"`
		Time string `json:"time"`
		Size string `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return err
	}

	// Parse remote entries
	slog.Info("fetched entries from remote", "count", len(items))

	// Store parsed items so we can write mapping after blobs are downloaded
	type parsedEntry struct {
		path string
		hash string
		time int64
	}
	parsed := make([]parsedEntry, 0, len(items))
	for _, item := range items {
		t, parseErr := time.Parse(time.RFC3339Nano, item.Time)
		if parseErr != nil {
			return fmt.Errorf("parse time %q: %v", item.Time, parseErr)
		}
		parsed = append(parsed, parsedEntry{item.Path, item.Hash, t.UnixMilli()})
	}

	blobClient := client
	if tr, ok := client.Transport.(*http.Transport); ok {
		clone := tr.Clone()
		clone.DisableCompression = true
		blobClient = &http.Client{Transport: clone}
	} else if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := tr.Clone()
		clone.DisableCompression = true
		blobClient = &http.Client{Transport: clone}
	}

	seen := make(map[string]bool, len(items))
	var downloaded, skipped int
	for _, item := range items {
		if seen[item.Hash] {
			continue
		}
		seen[item.Hash] = true

		gzPath := blobPath(destDir, item.Hash, true)
		if _, err := os.Stat(gzPath); err == nil {
			skipped++
			continue // already exists locally
		}

		blobU, _ := url.Parse(host)
		blobU.Path, _ = url.JoinPath(blobU.Path, blobRemotePath(item.Hash, true))
		blobResp, err := blobClient.Get(blobU.String())
		if err != nil {
			return fmt.Errorf("download blob %s: %v", item.Hash, err)
		}
		if blobResp.StatusCode != http.StatusOK {
			blobResp.Body.Close()
			return fmt.Errorf("download blob %s: %s", item.Hash, blobResp.Status)
		}

		if err := os.MkdirAll(filepath.Dir(gzPath), 0o755); err != nil {
			blobResp.Body.Close()
			return err
		}
		tmp, tmpErr := os.CreateTemp(filepath.Dir(gzPath), "*.wasm.gz")
		if tmpErr != nil {
			blobResp.Body.Close()
			return tmpErr
		}
		tmpName := tmp.Name()
		if _, copyErr := io.Copy(tmp, blobResp.Body); copyErr != nil {
			blobResp.Body.Close()
			tmp.Close()
			os.Remove(tmpName)
			return fmt.Errorf("download blob %s: %v", item.Hash, copyErr)
		}
		blobResp.Body.Close()
		if err := tmp.Close(); err != nil {
			os.Remove(tmpName)
			return err
		}
		if err := os.Rename(tmpName, gzPath); err != nil {
			os.Remove(tmpName)
			return err
		}
		fi, _ := os.Stat(gzPath)
		var sizeMB float64
		if fi != nil {
			sizeMB = float64(fi.Size()) / (1024 * 1024)
		}
		slog.Info("downloaded blob", "hash", item.Hash, "size_mb", sizeMB)
		downloaded++
	}

	if downloaded > 0 || skipped > 0 {
		slog.Info("backup complete", "downloaded", downloaded, "skipped", skipped)
	}

	// Write mapping only after all blobs are successfully downloaded
	store := NewBlobStore(destDir)
	for _, p := range parsed {
		if err := store.SetWithTime(p.path, p.hash, p.time); err != nil {
			return err
		}
	}

	return nil
}

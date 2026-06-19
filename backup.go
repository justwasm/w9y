package w9y

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

func newBackupCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup [-data-dir data]",
		Short: "Backup all entries and blobs from remote server",
		Long:  `Backup all entries and blobs from remote server to local data directory.`,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return backupRemote(http.DefaultClient, defaultHost, dataDir)
		},
	}
	return cmd
}

func backupRemote(client *http.Client, host, destDir string) error {
	u, err := url.Parse(host)
	if err != nil {
		return err
	}
	u.Path, err = url.JoinPath(u.Path, "/api/entries")
	if err != nil {
		return err
	}

	resp, err := client.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var items []struct {
		Path  string `json:"path"`
		Hash  string `json:"hash"`
		Time  string `json:"time"`
		Size  string `json:"size"`
		Alias string `json:"alias,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return err
	}

	slog.Info("fetched entries from remote", "count", len(items))

	// Separate aliases from regular blob entries
	aliases := make(map[string]string) // alias path → target path
	type parsedEntry struct {
		path string
		hash string
		time int64
	}
	hashToEntries := make(map[string][]parsedEntry, len(items))

	for _, item := range items {
		if item.Alias != "" {
			aliases[item.Path] = item.Alias
			continue
		}
		t, parseErr := time.Parse(time.RFC3339Nano, item.Time)
		if parseErr != nil {
			return fmt.Errorf("parse time %q: %v", item.Time, parseErr)
		}
		hashToEntries[item.Hash] = append(hashToEntries[item.Hash], parsedEntry{item.Path, item.Hash, t.UnixMilli()})
	}

	blobClient := newNoCompressionClient(client)
	store := NewBlobStore(destDir)

	// Download blobs and create blob symlinks
	var downloaded, skipped int
	for sha, entries := range hashToEntries {
		gzPath := blobPath(destDir, sha, true)
		if _, err := os.Stat(gzPath); err == nil {
			skipped++
		} else {
			blobU, _ := url.Parse(host)
			blobU.Path, _ = url.JoinPath(blobU.Path, blobRemotePath(sha, true))
			blobResp, err := blobClient.Get(blobU.String())
			if err != nil {
				return fmt.Errorf("download blob %s: %v", sha, err)
			}
			if blobResp.StatusCode != http.StatusOK {
				blobResp.Body.Close()
				return fmt.Errorf("download blob %s: %s", sha, blobResp.Status)
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
				return fmt.Errorf("download blob %s: %v", sha, copyErr)
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
			slog.Info("downloaded blob", "hash", sha, "size_mb", sizeMB)
			downloaded++
		}

		for _, e := range entries {
			if err := store.SetWithTime(e.path, e.hash, e.time); err != nil {
				return fmt.Errorf("write entry %s: %v", e.path, err)
			}
		}
	}

	// Create alias symlinks locally
	for alias, target := range aliases {
		if err := store.SetAlias(alias, target); err != nil {
			return fmt.Errorf("write alias %s -> %s: %v", alias, target, err)
		}
	}

	if downloaded > 0 || skipped > 0 || len(aliases) > 0 {
		slog.Info("backup complete", "downloaded", downloaded, "skipped", skipped, "entries", len(items)-len(aliases), "aliases", len(aliases))
	}

	return nil
}

package w9y

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newRestoreCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restore [-data-dir data]",
		Short: "Restore all entries and blobs to remote server",
		Long:  `Restore all entries and blobs from local data directory to remote server.`,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return restoreRemote(http.DefaultClient, defaultHost, dataDir)
		},
	}
	return cmd
}

func restoreRemote(client *http.Client, host, backupDir string) error {
	store := NewBlobStore(backupDir)
	entries, err := store.List()
	if err != nil {
		return err
	}

	slog.Info("restoring entries", "count", len(entries), "dir", backupDir)

	type blobEntry struct {
		path string
		time int64
	}
	shaToEntries := make(map[string][]blobEntry, len(entries))
	for p, e := range entries {
		shaToEntries[e.Hash] = append(shaToEntries[e.Hash], blobEntry{p, e.Time})
	}

	var uploaded, linked int
	for sha, entries := range shaToEntries {
		gzPath := blobPath(backupDir, sha, true)
		blobRemote := blobRemotePath(sha, true)
		exists, err := remoteFileExists(client, host, blobRemote)
		if err != nil {
			return err
		}

		// Only open the file when we actually need to upload it
		var f *os.File
		var fi os.FileInfo
		if !exists {
			f, err = os.Open(gzPath)
			if err != nil {
				return fmt.Errorf("missing blob %s in backup: %v", sha, err)
			}
			fi, err = f.Stat()
			if err != nil {
				f.Close()
				return fmt.Errorf("stat blob %s: %v", sha, err)
			}
		}

		for _, be := range entries {
			u, _ := url.Parse(host)
			u.Path, _ = url.JoinPath(u.Path, be.path)
			u.RawQuery = "hash=" + sha

			var body io.Reader
			if exists {
				body = http.NoBody
			} else {
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					f.Close()
					return fmt.Errorf("seek blob %s: %v", sha, err)
				}
				body = f
			}

			req, err := http.NewRequest(http.MethodPut, u.String(), body)
			if err != nil {
				if f != nil {
					f.Close()
				}
				return fmt.Errorf("restore %s: %v", be.path, err)
			}
			if !exists {
				req.ContentLength = fi.Size()
			}
			req.Header.Set("Content-Type", "application/gzip")
			req.Header.Set("Content-Encoding", "gzip")

			resp, err := client.Do(req)
			if err != nil {
				if f != nil {
					f.Close()
				}
				return fmt.Errorf("restore %s: %v", be.path, err)
			}
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				if f != nil {
					f.Close()
				}
				return fmt.Errorf("restore %s: %s: %s", be.path, resp.Status, strings.TrimSpace(string(bodyBytes)))
			}

			verb := "linked"
			if !exists {
				verb = "uploaded"
			}
			slog.Info(verb+" entry", "path", be.path, "blob", sha)
			if exists {
				linked++
			} else {
				uploaded++
			}
			exists = true
		}
		if f != nil {
			f.Close()
		}
	}

	slog.Info("restore complete", "uploaded", uploaded, "linked", linked)
	return nil
}

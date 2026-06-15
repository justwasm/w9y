package w9y

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	dataDir := fs.String("data-dir", getenv("DATA_DIR", "data"), "backup source directory")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: w9y restore [-data-dir data] [backup-dir]

Restore all entries and blobs from a local backup to the remote server.

flags:
  -data-dir  backup source directory (default data)`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	host := defaultHost
	backupDir := fs.Arg(0)
	if backupDir == "" {
		backupDir = *dataDir
	}
	if fs.NArg() > 1 {
		return errors.New("restore takes at most one argument (backup directory)")
	}
	return restoreRemote(http.DefaultClient, host, backupDir)
}

func restoreRemote(client *http.Client, host, backupDir string) error {
	m, err := loadMapping(backupDir)
	if err != nil {
		return err
	}

	slog.Info("restoring entries", "count", len(m.Entries), "dir", backupDir)

	type blobEntry struct {
		path string
		time int64
	}
	shaToEntries := make(map[string][]blobEntry, len(m.Entries))
	for p, e := range m.Entries {
		shaToEntries[e.SHA] = append(shaToEntries[e.SHA], blobEntry{p, e.Time})
	}

	var uploaded, linked int
	for sha, entries := range shaToEntries {
		gzPath := blobPath(backupDir, sha, true)
		gzData, err := os.ReadFile(gzPath)
		if err != nil {
			return fmt.Errorf("missing blob %s in backup: %v", sha, err)
		}

		blobRemote := blobRemotePath(sha, true)
		exists, err := remoteFileExists(client, host, blobRemote)
		if err != nil {
			return err
		}

		for _, be := range entries {
			u, _ := url.Parse(host)
			u.Path, _ = url.JoinPath(u.Path, be.path)
			u.RawQuery = "sha256=" + sha

			var body io.Reader
			if exists {
				body = http.NoBody
			} else {
				body = bytes.NewReader(gzData)
			}

			req, err := http.NewRequest(http.MethodPut, u.String(), body)
			if err != nil {
				return fmt.Errorf("restore %s: %v", be.path, err)
			}
			req.Header.Set("Content-Type", "application/gzip")
			req.Header.Set("Content-Encoding", "gzip")

			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("restore %s: %v", be.path, err)
			}
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
	}

	slog.Info("restore complete", "uploaded", uploaded, "linked", linked)
	return nil
}

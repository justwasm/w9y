package w9y

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("restore requires exactly one backup directory")
	}
	host := os.Getenv("HOST")
	if host == "" {
		return errors.New("HOST is required for restore")
	}
	return restoreRemote(http.DefaultClient, host, fs.Arg(0))
}

func restoreRemote(client *http.Client, host, backupDir string) error {
	m, err := loadMapping(backupDir)
	if err != nil {
		return err
	}

	type blobEntry struct {
		path string
		time int64
	}
	shaToEntries := make(map[string][]blobEntry, len(m.Entries))
	for p, e := range m.Entries {
		shaToEntries[e.SHA] = append(shaToEntries[e.SHA], blobEntry{p, e.Time})
	}

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

			var body io.Reader
			if exists {
				body = http.NoBody
				u.RawQuery = "sha=" + sha
			} else {
				body = bytes.NewReader(gzData)
			}

			req, err := http.NewRequest(http.MethodPut, u.String(), body)
			if err != nil {
				return err
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

			exists = true
		}
	}

	return nil
}

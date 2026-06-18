package w9y

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newUploadCommand() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "upload [--to /name.wasm] file.wasm",
		Short: "Upload a .wasm file to the remote server",
		Long: `Upload a .wasm file to the remote server.

The file is hashed, gzip-compressed, and uploaded. If the blob already
exists on the server, only a linking request is sent.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			fileName := args[0]
			remotePath := to
			if remotePath == "" {
				remotePath = "/" + filepath.Base(fileName)
			}
			return uploadFile(http.DefaultClient, fileName, remotePath)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "remote path (default /<filename>)")
	return cmd
}

func uploadFile(client *http.Client, fileName, remotePath string) error {
	f, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer f.Close()

	// Stream once to compute SHA256 of uncompressed content
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	sha := hex.EncodeToString(h.Sum(nil))

	host := defaultHost

	blobRemote := blobRemotePath(sha, true)
	exists, err := remoteFileExists(client, host, blobRemote)
	if err != nil {
		return err
	}

	ep := remotePath
	var body io.Reader
	bodyLen := int64(0)
	if exists {
		body = http.NoBody
	} else {
		// Rewind and gzip-compress to a temp file
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		tmp, tmpErr := os.CreateTemp("", "*.wasm.gz")
		if tmpErr != nil {
			return tmpErr
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		gzw, err := gzip.NewWriterLevel(tmp, gzip.BestCompression)
		if err != nil {
			return err
		}
		rawLen, copyErr := io.Copy(gzw, f)
		gzw.Close()
		tmp.Close()
		if copyErr != nil {
			return copyErr
		}
		// Reopen temp file for the upload body
		tmpR, openErr := os.Open(tmpName)
		if openErr != nil {
			return openErr
		}
		defer tmpR.Close()
		fi, statErr := tmpR.Stat()
		if statErr != nil {
			return statErr
		}
		bodyLen = fi.Size()
		body = tmpR
		slog.Info("compressing file", "name", fileName, "before_mb", float64(rawLen)/(1024*1024), "after_mb", float64(bodyLen)/(1024*1024))
	}

	u, err := url.Parse(host)
	if err != nil {
		return err
	}
	u.Path, err = url.JoinPath(u.Path, ep)
	if err != nil {
		return err
	}
	u.RawQuery = "hash=" + sha

	req, err := http.NewRequest(http.MethodPut, u.String(), body)
	if err != nil {
		return err
	}
	if token := loadToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if !exists {
		req.ContentLength = bodyLen
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload failed: %s: %s", resp.Status, strings.TrimSpace(string(bodyBytes)))
	}
	fmt.Printf("%s\n", strings.TrimSpace(string(bodyBytes)))
	return nil
}

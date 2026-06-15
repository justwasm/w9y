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
	"path/filepath"
	"strings"
)

func upload(args []string) error {
	return uploadWithClient(args, http.DefaultClient)
}

func uploadWithClient(args []string, client *http.Client) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	to := fs.String("to", "", "remote path to upload to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("upload requires exactly one file")
	}

	fileName := fs.Arg(0)
	remotePath := *to
	if remotePath == "" {
		remotePath = "/" + filepath.Base(fileName)
	}

	wasm, err := os.ReadFile(fileName)
	if err != nil {
		return err
	}
	sha := sha256Hex(wasm)

	host := defaultHost

	blobRemote := blobRemotePath(sha, true)
	exists, err := remoteFileExists(client, host, blobRemote)
	if err != nil {
		return err
	}

	ep := remotePath
	var body io.Reader
	if exists {
		body = http.NoBody
	} else {
		gz, err := gzipBytes(wasm)
		if err != nil {
			return err
		}
		beforeMB := float64(len(wasm)) / (1024 * 1024)
		afterMB := float64(len(gz)) / (1024 * 1024)
		fmt.Fprintf(os.Stderr, "compressing %s, %.2f MB -> %.2f MB\n", fileName, beforeMB, afterMB)
		body = bytes.NewReader(gz)
	}

	u, err := url.Parse(host)
	if err != nil {
		return err
	}
	u.Path, err = url.JoinPath(u.Path, ep)
	if err != nil {
		return err
	}
	if exists {
		u.RawQuery = "sha=" + sha
	}

	req, err := http.NewRequest(http.MethodPut, u.String(), body)
	if err != nil {
		return err
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

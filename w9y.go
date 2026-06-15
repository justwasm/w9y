package w9y

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultDataDir = "data"

var defaultHost = cmp.Or(os.Getenv("W9Y"), "https://w9y.up.railway.app/")

type entry struct {
	SHA  string `yaml:"sha" json:"sha"`
	Time int64  `yaml:"time" json:"time"`
}

type mapping struct {
	Entries map[string]entry `yaml:"entries"`
}

// Run starts server mode when PORT is set, otherwise it runs the CLI.
func Run(args []string) error {
	if port := os.Getenv("PORT"); port != "" {
		dataDir := getenv("DATA_DIR", defaultDataDir)
		addr := ":" + port
		slog.Info("starting server", "data_dir", dataDir, "addr", addr)

		// Integrity check on startup — errors are logged, not fatal
		if err := checkData(dataDir); err != nil {
			slog.Warn("integrity check found issues", "error", err)
		}

		return http.ListenAndServe(addr, NewServer(dataDir))
	}

	if len(args) == 0 {
		return usage()
	}

	var err error
	switch args[0] {
	case "upload":
		err = upload(args[1:])
	case "backup":
		err = backup(args[1:])
	case "restore":
		err = restore(args[1:])
	case "gc":
		err = gc(args[1:])
	case "check":
		err = check(args[1:])
	case "-h", "--help", "help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func usage() error {
	fmt.Fprintln(os.Stderr, `usage:
  w9y upload [--to /name.wasm] file.wasm
  w9y backup [-data-dir data] [dest-dir]
  w9y restore [-data-dir data] [backup-dir]
  w9y gc [-data-dir data] [-clean]
  w9y check [-data-dir data]

env:
  PORT      start server mode on this port when present
  DATA_DIR  data directory (default data)
  W9Y       remote server URL (default https://w9y.up.railway.app/)`)
	return nil
}

func remoteFileExists(client *http.Client, host, remotePath string) (bool, error) {
	u, err := url.Parse(host)
	if err != nil {
		return false, err
	}
	u.Path, err = url.JoinPath(u.Path, remotePath)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest(http.MethodHead, u.String(), nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("blob check failed: %s", resp.Status)
	}
}

func loadMapping(dataDir string) (*mapping, error) {
	mappingPath := filepath.Join(dataDir, "mapping.yaml")
	data, err := os.ReadFile(mappingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &mapping{Entries: make(map[string]entry)}, nil
		}
		return nil, err
	}
	var m mapping
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Entries == nil {
		m.Entries = make(map[string]entry)
	}
	return &m, nil
}

func saveMapping(dataDir string, m *mapping) error {
	mappingPath := filepath.Join(dataDir, "mapping.yaml")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(mappingPath, data, 0o644)
}

func gzipBytes(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gz.Write(src); err != nil {
		gz.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func cleanRemotePath(remotePath string) (string, error) {
	u, err := url.Parse(remotePath)
	if err != nil {
		return "", err
	}
	p := u.Path
	if p == "" {
		return "", fmt.Errorf("invalid remote path %q", remotePath)
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p, nil
}

func storagePath(dataDir, remotePath string) string {
	return filepath.Join(dataDir, filepath.FromSlash(strings.TrimPrefix(remotePath, "/")))
}

func blobPath(dataDir, sha string, gz bool) string {
	return storagePath(dataDir, blobRemotePath(sha, gz))
}

func blobRemotePath(sha string, gz bool) string {
	name := "/blob/" + sha + ".wasm"
	if gz {
		name += ".gz"
	}
	return name
}

func isBlobRemotePath(remotePath string) bool {
	return remotePath == "/blob" || strings.HasPrefix(remotePath, "/blob/")
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func contentType(remotePath string) string {
	if path.Ext(remotePath) == ".wasm" {
		return "application/wasm"
	}
	if typ := mime.TypeByExtension(path.Ext(remotePath)); typ != "" {
		return typ
	}
	return "application/octet-stream"
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

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
	"sync"

	"gopkg.in/yaml.v3"
)

const defaultDataDir = "data"

var defaultHost = cmp.Or(os.Getenv("W9Y"), "https://w9y.up.railway.app/")

var ErrPathNotFound = errors.New("path not found")

type Blob struct {
	Hash string `yaml:"hash" json:"hash"`
	Size int64  `yaml:"size" json:"size"`
	Time int64  `yaml:"time" json:"time"`
}

// BlobStore provides concurrent-safe access to the path→blob mapping.
type BlobStore interface {
	Get(path string) (Blob, error)
	Set(path string, blob Blob) error
	List() (map[string]Blob, error)
}

// NewBlobStore returns a file-based BlobStore backed by mapping.yaml in dataDir.
func NewBlobStore(dataDir string) BlobStore {
	return &fileBlobStore{dataDir: dataDir}
}

type fileBlobStore struct {
	dataDir string
	mu      sync.RWMutex
}

func (s *fileBlobStore) Get(path string) (Blob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := s.load()
	if err != nil {
		return Blob{}, err
	}
	b, ok := entries[path]
	if !ok {
		return Blob{}, ErrPathNotFound
	}
	return b, nil
}

func (s *fileBlobStore) Set(path string, blob Blob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return err
	}
	entries[path] = blob
	return s.save(entries)
}

func (s *fileBlobStore) List() (map[string]Blob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := s.load()
	if err != nil {
		return nil, err
	}
	result := make(map[string]Blob, len(entries))
	for k, v := range entries {
		result[k] = v
	}
	return result, nil
}

func (s *fileBlobStore) load() (map[string]Blob, error) {
	mappingPath := filepath.Join(s.dataDir, "mapping.yaml")
	data, err := os.ReadFile(mappingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]Blob), nil
		}
		return nil, err
	}
	var m struct {
		Entries map[string]Blob `yaml:"entries"`
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Entries == nil {
		m.Entries = make(map[string]Blob)
	}
	return m.Entries, nil
}

func (s *fileBlobStore) save(entries map[string]Blob) error {
	mappingPath := filepath.Join(s.dataDir, "mapping.yaml")
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return err
	}
	m := struct {
		Entries map[string]Blob `yaml:"entries"`
	}{Entries: entries}
	data, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(mappingPath, data, 0o644)
}

// Run starts server mode when the "server" subcommand is used,
// otherwise it dispatches to a CLI subcommand.
func Run(args []string) error {
	if len(args) == 0 {
		return usage()
	}

	switch args[0] {
	case "server":
		return runServer(args[1:])
	case "upload":
		return runCommand(upload, args[1:])
	case "backup":
		return runCommand(backup, args[1:])
	case "restore":
		return runCommand(restore, args[1:])
	case "gc":
		return runCommand(gc, args[1:])
	case "check":
		return runCommand(check, args[1:])
	case "-h", "--help", "help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	doCheck := fs.Bool("check", false, "run blob integrity check on startup")
	port := fs.String("port", getenv("PORT", "8080"), "server port")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: w9y server [-port port] [-check]

Start the w9y HTTP server.

flags:
  -port   server port (default $PORT or 8080)
  -check  run blob integrity check on startup`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	dataDir := getenv("DATA_DIR", defaultDataDir)
	addr := ":" + *port
	slog.Info("starting server", "data_dir", dataDir, "addr", addr)

	if *doCheck {
		if err := checkData(dataDir); err != nil {
			slog.Warn("integrity check found issues", "error", err)
		}
	}

	return http.ListenAndServe(addr, NewServer(dataDir))
}

func runCommand(cmd func([]string) error, args []string) error {
	err := cmd(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func usage() error {
	fmt.Fprintln(os.Stderr, `usage:
  w9y server [-port port] [-check]
  w9y upload [--to /name.wasm] file.wasm
  w9y backup [-data-dir data]
  w9y restore [-data-dir data]
  w9y gc [-data-dir data] [-clean]
  w9y check [-data-dir data]

env:
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

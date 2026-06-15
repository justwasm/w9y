package w9y

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const defaultDataDir = "data"

var defaultHost = cmp.Or(os.Getenv("W9Y"), "https://w9y.up.railway.app/")

var dataDir string

var ErrPathNotFound = errors.New("path not found")

type Blob struct {
	Hash string `yaml:"hash" json:"hash"`
	Time int64  `yaml:"time" json:"time"`
}

// BlobStore provides concurrent-safe access to the path→blob mapping.
type BlobStore interface {
	Get(path string) (Blob, error)
	Set(path, hash string) error
	SetWithTime(path, hash string, time int64) error
	SetBatch(entries map[string]Blob) error
	List() (map[string]Blob, error)
	DataDir() string
}

// NewBlobStore returns a file-based BlobStore backed by mapping.yaml in dataDir.
func NewBlobStore(dataDir string) BlobStore {
	s := &fileBlobStore{dataDir: dataDir}
	if err := s.loadCache(); err != nil {
		s.cache = make(map[string]Blob)
	}
	return s
}

func (s *fileBlobStore) loadCache() error {
	mappingPath := filepath.Join(s.dataDir, "mapping.yaml")
	data, err := os.ReadFile(mappingPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.cache = make(map[string]Blob)
			return nil
		}
		return err
	}
	var m struct {
		Entries map[string]Blob `yaml:"entries"`
	}
	if err := yaml.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("corrupt mapping.yaml: %w", err)
	}
	if m.Entries == nil {
		m.Entries = make(map[string]Blob)
	}
	s.cache = m.Entries
	return nil
}

type fileBlobStore struct {
	dataDir string
	mu      sync.RWMutex
	cache   map[string]Blob // in-memory cache, nil = not yet loaded
}

func (s *fileBlobStore) Get(path string) (Blob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.cache[path]
	if !ok {
		return Blob{}, ErrPathNotFound
	}
	return b, nil
}

func (s *fileBlobStore) Set(path, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[path] = Blob{Hash: hash, Time: time.Now().UnixMilli()}
	return s.save(s.cache)
}

func (s *fileBlobStore) SetWithTime(path, hash string, time int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[path] = Blob{Hash: hash, Time: time}
	return s.save(s.cache)
}

func (s *fileBlobStore) SetBatch(entries map[string]Blob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range entries {
		s.cache[k] = v
	}
	return s.save(s.cache)
}

func (s *fileBlobStore) DataDir() string { return s.dataDir }

func (s *fileBlobStore) List() (map[string]Blob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]Blob, len(s.cache))
	for k, v := range s.cache {
		result[k] = v
	}
	return result, nil
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
	if err := os.WriteFile(mappingPath, data, 0o644); err != nil {
		return err
	}
	s.cache = entries
	return nil
}

// NewRootCommand returns the root cobra.Command for the w9y CLI.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "w9y [command] [flags] [args]",
		Short: "WebAssembly blob hosting service",
		Long: `w9y is a WebAssembly blob hosting service with content-addressed storage,
automatic gzip serving, and on-demand Go WASM builds.`,
	}

	root.PersistentFlags().StringVar(&dataDir, "data-dir", getenv("DATA_DIR", defaultDataDir), "data directory")

	root.AddCommand(newServerCommand())
	root.AddCommand(newUploadCommand())
	root.AddCommand(newBackupCommand())
	root.AddCommand(newRestoreCommand())
	root.AddCommand(newGCCommand())
	root.AddCommand(newCheckCommand())

	return root
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
	// Reject path traversal
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("invalid remote path %q", remotePath)
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

// streamToTemp streams body into a temp file in dir, closes the file,
// and returns the temp file path along with how many bytes were written.
func streamToTemp(dir, pattern string, body io.Reader) (tmpName string, written int64, err error) {
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", 0, err
	}
	tmpName = tmp.Name()
	written, err = io.Copy(tmp, body)
	tmp.Close()
	if err != nil {
		os.Remove(tmpName)
		return "", 0, err
	}
	return tmpName, written, nil
}

// gzipSHA256 opens the gzip file, decompresses it, and returns the SHA-256
// hex digest of the decompressed content.
func gzipSHA256(gzPath string) (string, error) {
	f, err := os.Open(gzPath)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("decompress: %w", err)
	}
	defer gr.Close()

	h := sha256.New()
	if _, err := io.Copy(h, gr); err != nil {
		return "", fmt.Errorf("hash: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// newNoCompressionClient returns an HTTP client with transport-level
// compression disabled, so that gzip content is delivered as raw bytes
// rather than transparently decompressed by the transport layer.
func newNoCompressionClient(client *http.Client) *http.Client {
	if tr, ok := client.Transport.(*http.Transport); ok {
		clone := tr.Clone()
		clone.DisableCompression = true
		return &http.Client{Transport: clone}
	}
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := tr.Clone()
		clone.DisableCompression = true
		return &http.Client{Transport: clone}
	}
	return client
}

// blobFile represents a single .wasm.gz file in the blob directory.
type blobFile struct {
	Name string // filename (e.g. "abc123.wasm.gz")
	SHA  string // SHA256 hex without extension
	Path string // full filesystem path
}

// listBlobFiles returns all .wasm.gz files in dataDir/blob.
func listBlobFiles(dataDir string) ([]blobFile, error) {
	blobDir := filepath.Join(dataDir, "blob")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []blobFile
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		sha, ok := strings.CutSuffix(name, ".wasm.gz")
		if !ok {
			continue
		}
		files = append(files, blobFile{
			Name: name,
			SHA:  sha,
			Path: filepath.Join(blobDir, name),
		})
	}
	return files, nil
}

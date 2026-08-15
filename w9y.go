package w9y

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const defaultDataDir = "data"
const pathsDirName = "paths"

var defaultHost = cmp.Or(os.Getenv("W9Y"), "https://w9y.io/")

var dataDir string

var ErrPathNotFound = errors.New("path not found")

// wasmReplacements are go mod edit -replace directives applied before tidy
// to swap packages with WASM-compatible forks.
var wasmReplacements = []string{
	"github.com/atotto/clipboard=github.com/justwasm/clipboard@v0.1.6",
	"charm.land/bubbletea/v2=github.com/bubbletui/bubbletea/v2@v2.0.15",
}

// runGoModReplace runs "go mod edit -replace ..." with wasmReplacements.
func runGoModReplace(ctx context.Context, dir string) error {
	args := []string{"mod", "edit"}
	for _, r := range wasmReplacements {
		args = append(args, "-replace", r)
	}
	replaceCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(replaceCtx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stderr = new(bytes.Buffer)
	slog.Info("go mod edit -replace", "dir", dir)
	if err := cmd.Run(); err != nil {
		stderr := cmd.Stderr.(*bytes.Buffer).String()
		if stderr != "" {
			fmt.Fprint(os.Stderr, stderr)
		}
		return fmt.Errorf("go mod edit -replace: %w", err)
	}
	return nil
}

type Blob struct {
	Hash string `json:"hash"`
	Time int64  `json:"time"`
}

// BlobStore provides access to the path→blob mapping backed by symlinks.
type BlobStore interface {
	Get(path string) (Blob, error)
	Set(path, hash string) error
	SetWithTime(path, hash string, time int64) error
	Delete(path string) error
	List() (map[string]Blob, error)
	SetAlias(alias, target string) error
	GetAlias(alias string) (string, error)
	ListAliases() (map[string]string, error)
	DataDir() string
}

// NewBlobStore returns a symlink-backed BlobStore.
//
// Each path entry is a symlink under <dataDir>/paths/ pointing to
// ../blob/<sha256>.wasm.gz. The symlink's own mtime records the upload
// time. Aliases are symlinks whose target is another path (not a blob).
func NewBlobStore(dataDir string) BlobStore {
	return &symlinkBlobStore{dataDir: dataDir}
}

// symlinkBlobStore stores path→blob mappings as filesystem symlinks.
type symlinkBlobStore struct {
	dataDir string
}

func (s *symlinkBlobStore) storePath(remotePath string) string {
	clean := strings.TrimPrefix(remotePath, "/")
	return filepath.Join(s.dataDir, pathsDirName, filepath.FromSlash(clean))
}

// blobSymlinkTarget returns the relative symlink target for a blob at the
// given directory depth under dataDir/paths/.
func blobSymlinkTarget(hash string, depth int) string {
	parts := make([]string, depth)
	for i := range parts {
		parts[i] = ".."
	}
	parts = append(parts, "blob", hash+".wasm.gz")
	return filepath.Join(parts...)
}

// isBlobSymlink returns true if the (already Clean'd) symlink target
// points into a blob/ directory.
func isBlobSymlink(target string) bool {
	return filepath.Base(filepath.Dir(target)) == "blob"
}

// hashFromSymlink extracts the SHA-256 hex from a blob symlink target.
func hashFromSymlink(target string) string {
	cleaned := filepath.Clean(target)
	sha, ok := strings.CutSuffix(filepath.Base(cleaned), ".wasm.gz")
	if !ok || len(sha) != 64 {
		return ""
	}
	return sha
}

// walkEntries iterates the paths/ directory, calling fn for each blob
// symlink (not aliases). If fn returns true, the entry is included in the
// returned map. An empty result (nil error) means the paths directory
// doesn't exist or is empty.
func (s *symlinkBlobStore) walkEntries(fn func(remotePath, sha string, mtime time.Time) bool) (map[string]Blob, error) {
	pathsDir := filepath.Join(s.dataDir, pathsDirName)
	result := make(map[string]Blob)

	if _, err := os.Stat(pathsDir); os.IsNotExist(err) {
		return result, nil
	}

	err := filepath.WalkDir(pathsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible entries
		}
		if d.IsDir() || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return nil
		}
		if !isBlobSymlink(filepath.Clean(target)) {
			return nil
		}
		sha := hashFromSymlink(target)
		if sha == "" {
			return nil
		}
		rel, err := filepath.Rel(pathsDir, path)
		if err != nil {
			return nil
		}
		remotePath := "/" + filepath.ToSlash(rel)
		fi, err := os.Lstat(path)
		if err != nil {
			return nil
		}
		if fn != nil && !fn(remotePath, sha, fi.ModTime()) {
			return nil
		}
		result[remotePath] = Blob{Hash: sha, Time: s.readTimeFile(remotePath)}
		return nil
	})
	if os.IsNotExist(err) {
		return result, nil
	}
	return result, err
}

func (s *symlinkBlobStore) Get(path string) (Blob, error) {
	sp := s.storePath(path)
	target, err := os.Readlink(sp)
	if err != nil {
		if os.IsNotExist(err) {
			return Blob{}, ErrPathNotFound
		}
		return Blob{}, err
	}
	if !isBlobSymlink(filepath.Clean(target)) {
		return Blob{}, ErrPathNotFound
	}
	sha := hashFromSymlink(target)
	if sha == "" {
		return Blob{}, ErrPathNotFound
	}
	tm := s.readTimeFile(path)
	return Blob{Hash: sha, Time: tm}, nil
}

func (s *symlinkBlobStore) Set(path, hash string) error {
	return s.SetWithTime(path, hash, time.Now().UnixMilli())
}

func (s *symlinkBlobStore) SetWithTime(path, hash string, t int64) error {
	sp := s.storePath(path)
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		return err
	}

	rel, err := filepath.Rel(filepath.Join(s.dataDir, pathsDirName), filepath.Dir(sp))
	if err != nil {
		return err
	}
	depth := 1
	if rel != "." {
		depth = len(strings.Split(rel, string(filepath.Separator))) + 1
	}
	target := blobSymlinkTarget(hash, depth)

	os.Remove(sp)
	if err := os.Symlink(target, sp); err != nil {
		return err
	}

	// Store time in a sidecar file — os.Chtimes follows symlinks on
	// some platforms (macOS) and fails when the blob doesn't exist yet.
	return s.writeTimeFile(path, t)
}

func (s *symlinkBlobStore) Delete(path string) error {
	sp := s.storePath(path)
	if err := os.Remove(sp); err != nil {
		if os.IsNotExist(err) {
			return ErrPathNotFound
		}
		return err
	}
	os.Remove(s.timeFilePath(path))
	return nil
}

func (s *symlinkBlobStore) List() (map[string]Blob, error) {
	return s.walkEntries(nil)
}

func (s *symlinkBlobStore) SetAlias(alias, target string) error {
	sp := s.storePath(alias)
	if err := os.MkdirAll(filepath.Dir(sp), 0o755); err != nil {
		return err
	}
	// Create relative symlink: alias → target
	absTarget := filepath.Join(s.dataDir, pathsDirName, strings.TrimPrefix(target, "/"))
	relTarget, err := filepath.Rel(filepath.Dir(sp), absTarget)
	if err != nil {
		return err
	}
	os.Remove(sp)
	return os.Symlink(relTarget, sp)
}

func (s *symlinkBlobStore) GetAlias(alias string) (string, error) {
	sp := s.storePath(alias)
	target, err := os.Readlink(sp)
	if err != nil {
		return "", ErrPathNotFound
	}
	cleaned := filepath.Clean(target)
	if isBlobSymlink(cleaned) {
		return "", ErrPathNotFound
	}
	// Resolve relative to symlink's directory
	absTarget := filepath.Join(filepath.Dir(sp), cleaned)
	relTarget, err := filepath.Rel(filepath.Join(s.dataDir, pathsDirName), absTarget)
	if err != nil {
		return "", err
	}
	return "/" + filepath.ToSlash(relTarget), nil
}

func (s *symlinkBlobStore) ListAliases() (map[string]string, error) {
	pathsDir := filepath.Join(s.dataDir, pathsDirName)
	result := make(map[string]string)

	if _, err := os.Stat(pathsDir); os.IsNotExist(err) {
		return result, nil
	}

	err := filepath.WalkDir(pathsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return nil
		}
		cleaned := filepath.Clean(target)
		if isBlobSymlink(cleaned) {
			return nil
		}
		// Resolve alias target
		absTarget := filepath.Join(filepath.Dir(path), cleaned)
		relTarget, err := filepath.Rel(pathsDir, absTarget)
		if err != nil {
			return nil
		}
		aliasTarget := "/" + filepath.ToSlash(relTarget)

		// Only include if target is a valid blob entry
		if _, err := s.Get(aliasTarget); err != nil {
			return nil
		}

		rel, _ := filepath.Rel(pathsDir, path)
		result["/"+filepath.ToSlash(rel)] = aliasTarget
		return nil
	})
	if os.IsNotExist(err) {
		return result, nil
	}
	return result, err
}

func (s *symlinkBlobStore) DataDir() string { return s.dataDir }

func (s *symlinkBlobStore) timeFilePath(remotePath string) string {
	return s.storePath(remotePath) + ".time"
}

func (s *symlinkBlobStore) readTimeFile(remotePath string) int64 {
	data, err := os.ReadFile(s.timeFilePath(remotePath))
	if err != nil {
		// Fallback: use symlink mtime
		sp := s.storePath(remotePath)
		fi, err := os.Lstat(sp)
		if err != nil {
			return 0
		}
		return fi.ModTime().UnixMilli()
	}
	t, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return t
}

func (s *symlinkBlobStore) writeTimeFile(remotePath string, t int64) error {
	path := s.timeFilePath(remotePath)
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", t)), 0o644)
}

// NewRootCommand returns the root cobra.Command for the w9y CLI.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "w9y [command] [flags] [args]",
		Short: "WebAssembly blob hosting service",
		Long: `w9y is a WebAssembly blob hosting service with content-addressed storage,
automatic gzip serving, and on-demand Go WASM builds.`,
	}

	root.PersistentFlags().StringVar(&dataDir, "data-dir", cmp.Or(os.Getenv("DATA_DIR"), defaultDataDir), "data directory")

	root.AddCommand(newServerCommand())
	root.AddCommand(newUploadCommand())
	root.AddCommand(newBackupCommand())
	root.AddCommand(newRestoreCommand())
	root.AddCommand(newGCCommand())
	root.AddCommand(newCheckCommand())
	root.AddCommand(newBuildCommand())
	root.AddCommand(newResolveCommand())
	root.AddCommand(newListCommand())
	root.AddCommand(newGetCommand())
	root.AddCommand(newDeleteCommand())
	root.AddCommand(newLoginCommand())
	root.AddCommand(newLogoutCommand())
	root.AddCommand(newEnvCommand())
	root.AddCommand(newSemverCommand())
	root.AddCommand(newSearchCommand())
	root.AddCommand(newModCommand())

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

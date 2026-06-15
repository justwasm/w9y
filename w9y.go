package w9y

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	defaultDataDir = "data"

	headerBlobSHA256 = "X-W9Y-Blob-SHA256"
	headerLinkOnly   = "X-W9Y-Link-Only"
)

// Run starts server mode when PORT is set, otherwise it runs the CLI.
func Run(args []string) error {
	if port := os.Getenv("PORT"); port != "" {
		dataDir := getenv("DATA_DIR", defaultDataDir)
		addr := ":" + port
		log.Printf("w9y serving %s on %s", dataDir, addr)
		return http.ListenAndServe(addr, NewServer(dataDir))
	}

	if len(args) == 0 {
		return usage()
	}

	switch args[0] {
	case "upload":
		return upload(args[1:])
	case "backup":
		return backup(args[1:])
	case "restore":
		return restore(args[1:])
	case "-h", "--help", "help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() error {
	fmt.Fprintln(os.Stderr, `usage:
  w9y upload [--to /name.wasm] file.wasm
  w9y backup [--data-dir data] backup.tar
  w9y restore [--data-dir data] backup.tar

env:
  PORT      start server mode on this port when present
  DATA_DIR  server storage directory (default data)
  HOST      remote server endpoint for upload`)
	return nil
}

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
	remotePath, err := cleanUploadPath(remotePath)
	if err != nil {
		return err
	}

	host := os.Getenv("HOST")
	if host == "" {
		return errors.New("HOST is required for upload")
	}

	wasm, err := os.ReadFile(fileName)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(wasm)
	sha := hex.EncodeToString(sum[:])

	blobPath := blobRemotePath(sha, true)
	exists, err := remoteExists(client, host, blobPath)
	if err != nil {
		return err
	}

	var body io.Reader
	linkOnly := exists
	if linkOnly {
		body = http.NoBody
	} else {
		gz, err := gzipBytes(wasm)
		if err != nil {
			return err
		}
		body = bytes.NewReader(gz)
	}

	endpoint, err := joinEndpoint(host, remotePath)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(headerBlobSHA256, sha)
	if linkOnly {
		req.Header.Set(headerLinkOnly, "1")
	}

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

func remoteExists(client *http.Client, host, remotePath string) (bool, error) {
	endpoint, err := joinEndpoint(host, remotePath)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest(http.MethodHead, endpoint, nil)
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

func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dataDir := fs.String("data-dir", getenv("DATA_DIR", defaultDataDir), "data directory to back up")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("backup requires exactly one tarball path")
	}
	return backupData(*dataDir, fs.Arg(0))
}

func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	dataDir := fs.String("data-dir", getenv("DATA_DIR", defaultDataDir), "data directory to restore to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("restore requires exactly one tarball path")
	}
	return restoreData(*dataDir, fs.Arg(0))
}

func backupData(dataDir, tarball string) error {
	out, err := os.Create(tarball)
	if err != nil {
		return err
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	defer tw.Close()

	return filepath.WalkDir(dataDir, func(filePath string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == dataDir {
			return nil
		}
		rel, err := filepath.Rel(dataDir, filePath)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if skipBackupEntry(name) {
			return nil
		}

		info, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(filePath)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, in)
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func restoreData(dataDir, tarball string) error {
	in, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}

	tr := tar.NewReader(in)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		dst, err := safeTarPath(dataDir, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := validateSymlinkTarget(dataDir, dst, hdr.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported tar entry %q", hdr.Name)
		}
	}

	return restoreMissingWasmBlobs(dataDir)
}

func restoreMissingWasmBlobs(dataDir string) error {
	blobDir := filepath.Join(dataDir, "blob")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".wasm.gz") {
			continue
		}
		gzPath := filepath.Join(blobDir, name)
		wasmPath := strings.TrimSuffix(gzPath, ".gz")
		if _, err := os.Stat(wasmPath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := gunzipFile(gzPath, wasmPath); err != nil {
			return err
		}
	}
	return nil
}

func skipBackupEntry(name string) bool {
	return strings.HasPrefix(name, "blob/") && strings.HasSuffix(name, ".wasm") && !strings.HasSuffix(name, ".wasm.gz")
}

func safeTarPath(dataDir, name string) (string, error) {
	slashName := filepath.ToSlash(name)
	if path.IsAbs(slashName) {
		return "", fmt.Errorf("invalid tar path %q", name)
	}
	clean := path.Clean(slashName)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid tar path %q", name)
	}
	return filepath.Join(dataDir, filepath.FromSlash(clean)), nil
}

func validateSymlinkTarget(dataDir, linkPath, target string) error {
	if filepath.IsAbs(target) {
		return fmt.Errorf("refusing absolute symlink %q", target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
	rel, err := filepath.Rel(dataDir, resolved)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing symlink outside data dir %q", target)
	}
	return nil
}

// NewServer returns the w9y HTTP handler.
func NewServer(dataDir string) http.Handler {
	return withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		remotePath, err := cleanRemotePath(r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodPut, http.MethodPost:
			handleUpload(w, r, dataDir, remotePath)
		case http.MethodGet, http.MethodHead:
			handleDownload(w, r, dataDir, remotePath)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding, "+headerBlobSHA256+", "+headerLinkOnly)
		next.ServeHTTP(w, r)
	})
}

func handleUpload(w http.ResponseWriter, r *http.Request, dataDir, remotePath string) {
	remotePath, err := cleanUploadPath(remotePath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sha := r.Header.Get(headerBlobSHA256)
	if !validSHA256(sha) {
		http.Error(w, headerBlobSHA256+" is required", http.StatusBadRequest)
		return
	}

	if r.Header.Get(headerLinkOnly) == "1" {
		if _, err := os.Stat(blobPath(dataDir, sha, true)); err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "blob not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if err := storeCompressedBlob(dataDir, sha, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := linkPathToBlob(dataDir, remotePath, sha); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "uploaded %s -> %s", remotePath, blobRemotePath(sha, false))
}

func handleDownload(w http.ResponseWriter, r *http.Request, dataDir, remotePath string) {
	filePath := storagePath(dataDir, remotePath)
	resolved, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	gzPath := resolved + ".gz"
	if strings.HasSuffix(resolved, ".gz") {
		gzPath = resolved
	}
	file, err := os.Open(gzPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType(remotePath))
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")
	http.ServeContent(w, r, path.Base(remotePath)+".gz", stat.ModTime(), file)
}

func storeCompressedBlob(dataDir, sha string, body io.Reader) error {
	gzPath := blobPath(dataDir, sha, true)
	wasmPath := blobPath(dataDir, sha, false)
	if err := os.MkdirAll(filepath.Dir(gzPath), 0o755); err != nil {
		return err
	}

	var gz bytes.Buffer
	tee := io.TeeReader(body, &gz)
	gr, err := gzip.NewReader(tee)
	if err != nil {
		return fmt.Errorf("invalid gzip body: %w", err)
	}
	var wasm bytes.Buffer
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(&wasm, hasher), gr); err != nil {
		gr.Close()
		return err
	}
	if err := gr.Close(); err != nil {
		return err
	}
	if _, err := io.Copy(&gz, body); err != nil {
		return err
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != sha {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, sha)
	}

	if err := os.WriteFile(gzPath, gz.Bytes(), 0o644); err != nil {
		return err
	}
	return os.WriteFile(wasmPath, wasm.Bytes(), 0o644)
}

func linkPathToBlob(dataDir, remotePath, sha string) error {
	linkPath := storagePath(dataDir, remotePath)
	targetPath := blobPath(dataDir, sha, false)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return err
	}
	target, err := filepath.Rel(filepath.Dir(linkPath), targetPath)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(linkPath); err != nil {
		return err
	}
	return os.Symlink(target, linkPath)
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

func gunzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer gz.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, gz)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func cleanUploadPath(remotePath string) (string, error) {
	clean, err := cleanRemotePath(remotePath)
	if err != nil {
		return "", err
	}
	if isBlobRemotePath(clean) {
		return "", errors.New("uploads to /blob are not allowed")
	}
	return clean, nil
}

func cleanRemotePath(remotePath string) (string, error) {
	if remotePath == "" || remotePath == "/" {
		return "", errors.New("path is required")
	}
	if !strings.HasPrefix(remotePath, "/") {
		remotePath = "/" + remotePath
	}
	clean := path.Clean(remotePath)
	if clean == "/" || strings.Contains(clean, "../") || strings.Contains(clean, "/..") {
		return "", fmt.Errorf("invalid path %q", remotePath)
	}
	return clean, nil
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

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func contentType(remotePath string) string {
	if path.Ext(strings.TrimSuffix(remotePath, ".gz")) == ".wasm" {
		return "application/wasm"
	}
	if typ := mime.TypeByExtension(path.Ext(remotePath)); typ != "" {
		return typ
	}
	return "application/octet-stream"
}

func joinEndpoint(host, remotePath string) (string, error) {
	u, err := url.Parse(host)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid HOST %q", host)
	}
	u.Path = path.Join(u.Path, remotePath)
	return u.String(), nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

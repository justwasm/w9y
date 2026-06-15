package w9y

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultDataDir = "data"

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

	wasm, err := os.ReadFile(fileName)
	if err != nil {
		return err
	}
	gz, err := gzipBytes(wasm)
	if err != nil {
		return err
	}

	host := os.Getenv("HOST")
	if host == "" {
		return errors.New("HOST is required for upload")
	}
	endpoint, err := joinEndpoint(host, remotePath)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, endpoint, bytes.NewReader(gz))
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

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Printf("%s\n", strings.TrimSpace(string(body)))
	return nil
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

		info, err := os.Stat(filePath)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
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
		case tar.TypeReg:
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

	return nil
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

		if remotePath == "/" {
			handleList(w, r, dataDir)
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
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		next.ServeHTTP(w, r)
	})
}

func handleList(w http.ResponseWriter, r *http.Request, dataDir string) {
	m, err := loadMapping(dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type item struct {
		Path string `json:"path"`
		SHA  string `json:"sha"`
		Time int64  `json:"time"`
	}
	items := make([]item, 0, len(m.Entries))
	for p, e := range m.Entries {
		items = append(items, item{p, e.SHA, e.Time})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Time > items[j].Time
	})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(items)
}

func handleUpload(w http.ResponseWriter, r *http.Request, dataDir, remotePath string) {
	if isBlobRemotePath(remotePath) {
		http.Error(w, "uploads to /blob are not allowed", http.StatusBadRequest)
		return
	}

	gz, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sha := sha256Hex(gz)

	gzPath := blobPath(dataDir, sha, true)
	if err := os.MkdirAll(filepath.Dir(gzPath), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(gzPath, gz, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	m, err := loadMapping(dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m.Entries[remotePath] = entry{SHA: sha, Time: time.Now().UnixMilli()}
	if err := saveMapping(dataDir, m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "uploaded %s -> blob/%s.wasm.gz", remotePath, sha)
}

func handleDownload(w http.ResponseWriter, r *http.Request, dataDir, remotePath string) {
	// blob path serves the gzip file directly
	if isBlobRemotePath(remotePath) {
		gzPath := storagePath(dataDir, remotePath)
		serveGzipFile(w, r, gzPath)
		return
	}

	m, err := loadMapping(dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	e, ok := m.Entries[remotePath]
	if !ok {
		http.NotFound(w, r)
		return
	}
	gzPath := blobPath(dataDir, e.SHA, true)
	serveGzipFile(w, r, gzPath)
}

func serveGzipFile(w http.ResponseWriter, r *http.Request, gzPath string) {
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

	virtualPath := strings.TrimSuffix(gzPath, ".gz")
	w.Header().Set("Content-Type", contentType(virtualPath))
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")
	http.ServeContent(w, r, path.Base(virtualPath)+".gz", stat.ModTime(), file)
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
	if path.Ext(strings.TrimSuffix(remotePath, ".gz")) == ".wasm" {
		return "application/wasm"
	}
	if typ := mime.TypeByExtension(path.Ext(remotePath)); typ != "" {
		return typ
	}
	return "application/octet-stream"
}

func joinEndpoint(host, remotePath string) (string, error) {
	return url.JoinPath(host, remotePath)
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

package main

import (
	"compress/gzip"
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
	"time"
)

const defaultDataDir = "data"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if port := os.Getenv("PORT"); port != "" {
		dataDir := getenv("DATA_DIR", defaultDataDir)
		addr := ":" + port
		log.Printf("w9y serving %s on %s", dataDir, addr)
		return http.ListenAndServe(addr, newServer(dataDir))
	}

	if len(args) == 0 {
		return usage()
	}

	switch args[0] {
	case "upload":
		return upload(args[1:])
	case "-h", "--help", "help":
		return usage()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() error {
	fmt.Fprintln(os.Stderr, `usage:
  w9y upload [--as /name.wasm] file.wasm

env:
  PORT      start server mode on this port when present
  DATA_DIR  server storage directory (default data)
  HOST      remote server endpoint for upload`)
	return nil
}

func upload(args []string) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	as := fs.String("as", "", "remote path to upload as")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("upload requires exactly one file")
	}

	fileName := fs.Arg(0)
	remotePath := *as
	if remotePath == "" {
		remotePath = "/" + filepath.Base(fileName)
	}
	remotePath, err := cleanRemotePath(remotePath)
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

	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	req, err := http.NewRequest(http.MethodPut, endpoint, file)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType(remotePath))

	resp, err := http.DefaultClient.Do(req)
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

func newServer(dataDir string) http.Handler {
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
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		next.ServeHTTP(w, r)
	})
}

func handleUpload(w http.ResponseWriter, r *http.Request, dataDir, remotePath string) {
	dst := storagePath(dataDir, remotePath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	file, err := os.Create(dst)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, copyErr := io.Copy(file, r.Body)
	closeErr := file.Close()
	if copyErr != nil {
		http.Error(w, copyErr.Error(), http.StatusInternalServerError)
		return
	}
	if closeErr != nil {
		http.Error(w, closeErr.Error(), http.StatusInternalServerError)
		return
	}

	if err := gzipFile(dst, dst+".gz"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "uploaded %s", remotePath)
}

func handleDownload(w http.ResponseWriter, r *http.Request, dataDir, remotePath string) {
	gzPath := storagePath(dataDir, remotePath) + ".gz"
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

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	gz, err := gzip.NewWriterLevel(out, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.Name = path.Base(src)
	gz.ModTime = time.Now()
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		return err
	}
	return gz.Close()
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

func contentType(remotePath string) string {
	if path.Ext(remotePath) == ".wasm" {
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

package w9y

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const goproxyPrefix = "/goproxy/"

// isGoproxyPath returns true if the remotePath is a /goproxy/... path.
func isGoproxyPath(remotePath string) bool {
	return strings.HasPrefix(remotePath, goproxyPrefix)
}

// handleGoproxy implements the Go module proxy protocol for gist paths.
// GOPROXY protocol: https://go.dev/ref/mod#goproxy-protocol
//
// Accepted paths:
//
//	/goproxy/<module>/@v/list          → list versions
//	/goproxy/<module>/@v/<version>.info → version info
//	/goproxy/<module>/@v/<version>.mod  → go.mod
//	/goproxy/<module>/@v/<version>.zip  → source zip
//	/goproxy/<module>/@latest          → latest version info
func handleGoproxy(w http.ResponseWriter, r *http.Request, remotePath string) {
	trimmed := strings.TrimPrefix(remotePath, goproxyPrefix)

	atvIdx := strings.Index(trimmed, "/@v/")
	atLatest := strings.HasSuffix(trimmed, "/@latest")

	if atvIdx < 0 && !atLatest {
		http.Error(w, "bad goproxy path", http.StatusBadRequest)
		return
	}

	var modPath string
	if atvIdx >= 0 {
		modPath = trimmed[:atvIdx]
	} else {
		modPath = strings.TrimSuffix(trimmed, "/@latest")
	}

	// Return 404 for non-gist paths so the toolchain continues probing
	if !strings.HasPrefix(modPath, "gist.github.com/") {
		http.Error(w, "not a gist module", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	switch {
	case atLatest:
		handleGoproxyLatest(w, r, ctx, modPath)
	case atvIdx >= 0:
		suffix := trimmed[atvIdx+len("/@v/"):]
		switch {
		case suffix == "list":
			handleGoproxyList(w, r, ctx, modPath)
		case strings.HasSuffix(suffix, ".info"):
			handleGoproxyInfo(w, r, ctx, modPath, strings.TrimSuffix(suffix, ".info"))
		case strings.HasSuffix(suffix, ".mod"):
			handleGoproxyMod(w, r, ctx, modPath, strings.TrimSuffix(suffix, ".mod"))
		case strings.HasSuffix(suffix, ".zip"):
			handleGoproxyZip(w, r, ctx, modPath, strings.TrimSuffix(suffix, ".zip"))
		default:
			http.Error(w, "unknown goproxy endpoint", http.StatusBadRequest)
		}
	}
}

func handleGoproxyLatest(w http.ResponseWriter, r *http.Request, ctx context.Context, modPath string) {
	tmpDir, err := os.MkdirTemp("", "w9y-goproxy-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	commitHash, err := cloneGist(ctx, modPath, "", tmpDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	commitT, err := commitTime(ctx, tmpDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pv := pseudoVersion(commitT, commitHash)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{
		"Version": pv,
		"Time":    commitT.UTC().Format(time.RFC3339Nano),
	})
}

func handleGoproxyList(w http.ResponseWriter, r *http.Request, ctx context.Context, modPath string) {
	tmpDir, err := os.MkdirTemp("", "w9y-goproxy-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	commitHash, err := cloneGist(ctx, modPath, "", tmpDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	commitT, err := commitTime(ctx, tmpDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pv := pseudoVersion(commitT, commitHash)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(pv + "\n"))
}

func handleGoproxyInfo(w http.ResponseWriter, r *http.Request, ctx context.Context, modPath, version string) {
	tmpDir, err := os.MkdirTemp("", "w9y-goproxy-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	commitHash := commitFromVersion(version)
	if _, err := cloneGist(ctx, modPath, commitHash, tmpDir); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	commitT, err := commitTime(ctx, tmpDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	pv := pseudoVersion(commitT, commitHash)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{
		"Version": pv,
		"Time":    commitT.UTC().Format(time.RFC3339Nano),
	})
}

func handleGoproxyMod(w http.ResponseWriter, r *http.Request, ctx context.Context, modPath, version string) {
	tmpDir, err := os.MkdirTemp("", "w9y-goproxy-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	commitHash := commitFromVersion(version)
	if _, err := cloneGist(ctx, modPath, commitHash, tmpDir); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	modBytes, err := os.ReadFile(filepath.Join(tmpDir, "go.mod"))
	if err != nil {
		// No go.mod — return a minimal one with the correct module path
		modBytes = []byte("module " + modPath + "\n\ngo 1.21\n")
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(modBytes)
}

func handleGoproxyZip(w http.ResponseWriter, r *http.Request, ctx context.Context, modPath, version string) {
	tmpDir, err := os.MkdirTemp("", "w9y-goproxy-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	commitHash := commitFromVersion(version)
	if _, err := cloneGist(ctx, modPath, commitHash, tmpDir); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Ensure go.mod exists in the zip with the correct module path
	if _, err := os.Stat(filepath.Join(tmpDir, "go.mod")); os.IsNotExist(err) {
		modBytes := []byte("module " + modPath + "\n\ngo 1.21\n")
		if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), modBytes, 0644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Zip the gist directory — Go expects files under <module>@<version>/
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := modPath + "@" + version + "/"

	err = filepath.Walk(tmpDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Skip .git
		if fi.IsDir() && fi.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(tmpDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := prefix + filepath.ToSlash(rel)
		if fi.IsDir() {
			_, err := zw.Create(name + "/")
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := zw.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
	w.Write(buf.Bytes())
}

// handleGoGet serves the ?go-get=1 discovery endpoint for gist paths.
// This allows the Go toolchain to resolve gist import paths via direct VCS.
//
// Request: GET /gist.github.com/user/id?go-get=1
// Response: HTML with <meta name="go-import">
func handleGoGet(w http.ResponseWriter, r *http.Request, remotePath string) {
	importPath := strings.TrimPrefix(remotePath, "/")
	if !strings.HasPrefix(importPath, "gist.github.com/") {
		http.NotFound(w, r)
		return
	}

	// Extract user/id from gist.github.com/user/id
	parts := strings.SplitN(importPath, "/", 3)
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	gistRef := parts[1] + "/" + parts[2]
	repoURL := "https://gist.github.com/" + gistRef + ".git"

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
<meta name="go-import" content="%s git %s">
<meta name="go-source" content="%s git %s https://gist.github.com/%s{/dir} https://gist.github.com/%s{/dir}/{file}#L{line}">
</head>
<body>go get %s</body>
</html>
`, importPath, repoURL, importPath, repoURL, gistRef, gistRef, importPath)
}

// pseudoVersion generates a Go pseudo-version from a commit hash and time.
// Format: v0.0.0-<yyyymmddhhmmss>-<commit12>
func pseudoVersion(t time.Time, commitHash string) string {
	if len(commitHash) > 12 {
		commitHash = commitHash[:12]
	}
	return fmt.Sprintf("v0.0.0-%s-%s", t.UTC().Format("20060102150405"), commitHash)
}

// commitFromVersion extracts the commit hash from a version string.
// Handles both pseudo-versions (v0.0.0-<ts>-<commit12>) and raw commit hashes.
func commitFromVersion(version string) string {
	// If it looks like a raw commit hash, use it directly
	if len(version) >= 40 {
		for _, c := range version {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return version
			}
		}
		return version
	}
	// Pseudo-version format: <base>-<timestamp>-<commit12>
	// Commit is the part after the last hyphen
	if idx := strings.LastIndex(version, "-"); idx >= 0 {
		return version[idx+1:]
	}
	return version
}


func commitTime(ctx context.Context, repoDir string) (time.Time, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%ct")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}
	unix := strings.TrimSpace(string(out))
	sec, err := parseInt64(unix)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(sec, 0), nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %s", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

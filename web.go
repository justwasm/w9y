package w9y

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web/*
var webFS embed.FS

func webHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}

// webHasFile reports whether remotePath resolves to a regular file in the
// embedded web filesystem. Used to short-circuit non-root paths (e.g. /css/foo.css,
// /index.html) to webHandler() before falling through to the blob store.
func webHasFile(remotePath string) bool {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return false
	}
	name := strings.TrimPrefix(remotePath, "/")
	if name == "" {
		return false
	}
	f, err := sub.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return !stat.IsDir()
}

package w9y

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

var wasmMagic = []byte{0x00, 0x61, 0x73, 0x6d} // \0asm

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	dataDir := fs.String("data-dir", getenv("DATA_DIR", defaultDataDir), "data directory")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `usage: w9y check [-data-dir data]

Verify blob integrity: validate SHA256 and WASM magic bytes for every blob.

flags:
  -data-dir  data directory (default data)`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	return checkData(*dataDir)
}

func checkData(dataDir string) error {
	blobDir := filepath.Join(dataDir, "blob")
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("blob directory does not exist", "path", blobDir)
			return nil
		}
		return err
	}

	var total, passed, failed int
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		sha, ok := strings.CutSuffix(name, ".wasm.gz")
		if !ok {
			continue
		}
		total++
		path := filepath.Join(blobDir, name)

		if err := verifyBlob(path, sha); err != nil {
			slog.Error("blob check failed", "name", name, "error", err)
			failed++
		} else {
			slog.Info("blob check passed", "name", name)
			passed++
		}
	}

	slog.Info("check complete", "total", total, "passed", passed, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d blobs failed check", failed, total)
	}
	return nil
}

func verifyBlob(path, wantSHA string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gr.Close()

	// Check magic bytes — read just the first 4 bytes
	magic := make([]byte, len(wasmMagic))
	if _, err := io.ReadFull(gr, magic); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	for i, b := range wasmMagic {
		if magic[i] != b {
			return fmt.Errorf("bad magic: got %02x, want %02x (not a WASM binary)", magic, wasmMagic)
		}
	}

	// Stream the rest through SHA256
	h := sha256.New()
	if _, err := io.Copy(h, gr); err != nil {
		return fmt.Errorf("hash: %w", err)
	}

	gotSHA := hex.EncodeToString(h.Sum(nil))
	if gotSHA != wantSHA {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", gotSHA, wantSHA)
	}

	return nil
}

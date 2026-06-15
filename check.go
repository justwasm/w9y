package w9y

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

var wasmMagic = []byte{0x00, 0x61, 0x73, 0x6d} // \0asm

func newCheckCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check [-data-dir data]",
		Short: "Verify blob integrity",
		Long:  `Verify blob integrity: validate SHA256 and WASM magic bytes for every blob.`,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return checkData(dataDir)
		},
	}
	return cmd
}

func checkData(dataDir string) error {
	files, err := listBlobFiles(dataDir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		slog.Warn("blob directory does not exist or is empty", "path", filepath.Join(dataDir, "blob"))
		return nil
	}

	var total, passed, failed int
	for _, f := range files {
		total++
		if err := verifyBlob(f.Path, f.SHA); err != nil {
			slog.Error("blob check failed", "name", f.Name, "error", err)
			failed++
		} else {
			slog.Info("blob check passed", "name", f.Name)
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

	// Stream the full content (including magic bytes) through SHA256
	h := sha256.New()
	h.Write(magic)
	if _, err := io.Copy(h, gr); err != nil {
		return fmt.Errorf("hash: %w", err)
	}

	gotSHA := hex.EncodeToString(h.Sum(nil))
	if gotSHA != wantSHA {
		return fmt.Errorf("hash mismatch: got %s, want %s", gotSHA, wantSHA)
	}

	return nil
}

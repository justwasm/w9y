package w9y

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newGetCommand() *cobra.Command {
	var useTinyGo bool
	var outFile string

	cmd := &cobra.Command{
		Use:   "get <spec>",
		Short: "Download a WASM binary from the remote server",
		Long: `Download a WASM binary by requesting /<go|tinygo>/<spec> from the remote
w9y server and saving it locally.

Examples:
  w9y get golang.org/dl/gotip@latest
  w9y get --tinygo github.com/bubbletui/bubbletea/v2/examples/chat@v2.0.9
  w9y get -o my.wasm github.com/justwasm/w9y/cmd/w9y@latest
  w9y get gist.github.com/btwiuse/83e2efac358d22a009ece0a3f4feb801
`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			runtime := "go"
			if useTinyGo {
				runtime = "tinygo"
			}
			return runGet(args[0], runtime, outFile)
		},
	}
	cmd.Flags().BoolVar(&useTinyGo, "tinygo", false, "use TinyGo runtime instead of Go")
	cmd.Flags().StringVarP(&outFile, "output", "o", "", "output file name (default: <pkg>.wasm)")
	return cmd
}

func runGet(spec, runtime, outFile string) error {
	if runtime != "go" && runtime != "tinygo" {
		return fmt.Errorf("unsupported runtime %q (use go or tinygo)", runtime)
	}

	remotePath := "/" + runtime + "/" + spec

	if outFile == "" {
		outFile = defaultOutputName(spec)
	}

	slog.Info("downloading", "remote", remotePath, "output", outFile)

	u, err := url.Parse(defaultHost)
	if err != nil {
		return err
	}
	u.Path, err = url.JoinPath(u.Path, remotePath)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return err
	}
	if token := loadToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	tmp, tmpErr := os.CreateTemp("", "w9y-get-*.wasm")
	if tmpErr != nil {
		return tmpErr
	}
	tmpName := tmp.Name()

	n, err := io.Copy(tmp, resp.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmpName)
		return err
	}

	if err := os.Rename(tmpName, outFile); err != nil {
		os.Remove(tmpName)
		return err
	}

	fmt.Printf("%s (%.2fMB)\n", outFile, float64(n)/(1024*1024))
	return nil
}

// defaultOutputName extracts a reasonable filename from a spec.
// "golang.org/dl/gotip@latest" → "gotip.wasm"
// "gist.github.com/user/id" → "id.wasm"
func defaultOutputName(spec string) string {
	// Strip version if present
	name, _, _ := strings.Cut(spec, "@")
	parts := strings.Split(name, "/")
	base := parts[len(parts)-1]
	if !strings.HasSuffix(base, ".wasm") {
		base += ".wasm"
	}
	return base
}

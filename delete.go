package w9y

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func newDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <path>",
		Short: "Delete a mapping entry from the remote server",
		Long: `Delete a path→blob mapping from the remote w9y server.
The blob itself is not removed — run gc on the server to clean up orphans.

Examples:
  w9y delete /hush.wasm
  w9y delete /go/github.com/btwiuse/w9y/cmd/w9y@v0.0.1
`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runDelete(args[0])
		},
	}
}

func runDelete(remotePath string) error {
	if !strings.HasPrefix(remotePath, "/") {
		remotePath = "/" + remotePath
	}

	u, err := url.Parse(defaultHost)
	if err != nil {
		return err
	}
	u.Path, err = url.JoinPath(u.Path, remotePath)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodDelete, u.String(), nil)
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

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("delete failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	fmt.Println(strings.TrimSpace(string(body)))
	return nil
}

package w9y

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

func newListCommand() *cobra.Command {
	var outputJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List remote mapping entries",
		Long:  `List all path→blob mappings from the remote w9y server.`,
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return runList(outputJSON)
		},
	}
	cmd.Flags().BoolVar(&outputJSON, "json", false, "output as JSON")
	return cmd
}

func runList(outputJSON bool) error {
	u, err := url.Parse(defaultHost)
	if err != nil {
		return err
	}

	resp, err := http.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("list failed: %s", resp.Status)
	}

	var items []struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
		Time string `json:"time"`
		Size string `json:"size"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return err
	}

	if outputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	for _, item := range items {
		fmt.Printf("%s (%s)\n", item.Path, item.Size)
	}
	return nil
}

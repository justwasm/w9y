package w9y

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newSearchCommand() *cobra.Command {
	var outputJSON bool

	cmd := &cobra.Command{
		Use:   "search <keyword>",
		Short: "Search entries by keyword",
		Long:  `Search path→blob mappings by keyword (matches path and hash, case-insensitive).`,
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runSearch(args[0], outputJSON)
		},
	}
	cmd.Flags().BoolVar(&outputJSON, "json", false, "output as JSON")
	return cmd
}

func runSearch(keyword string, outputJSON bool) error {
	u, err := url.Parse(defaultHost)
	if err != nil {
		return err
	}

	u.Path, err = url.JoinPath(u.Path, "/api/entries")
	if err != nil {
		return err
	}

	resp, err := http.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("search failed: %s", resp.Status)
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

	lq := strings.ToLower(keyword)
	var matched []struct {
		Path string `json:"path"`
		Hash string `json:"hash"`
		Time string `json:"time"`
		Size string `json:"size"`
	}
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.Path), lq) ||
			strings.Contains(strings.ToLower(item.Hash), lq) {
			matched = append(matched, item)
		}
	}

	if outputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(matched)
	}

	if len(matched) == 0 {
		fmt.Println("No matches")
		return nil
	}
	for _, item := range matched {
		fmt.Printf("%s (%s)\n", item.Path, item.Size)
	}
	return nil
}

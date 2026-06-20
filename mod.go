package w9y

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newModCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mod <command> [flags] [args]",
		Short: "Manage manifests",
		Long: `Manage go.mod-like WASM manifests.

A manifest groups multiple output files with their Go import paths under a
single named version, using a go.mod-like format:

  module go
  go 1.26
  version v1.22.0

  bin/go    cmd/go
  bin/vet   golang.org/x/tools/cmd/vet

Commands:
  parse   Parse and display a manifest file
  create  Create a new manifest from arguments or stdin
  upload  Upload a manifest to the server`,
	}

	cmd.AddCommand(newModParseCommand())
	cmd.AddCommand(newModFmtCommand())
	cmd.AddCommand(newModApplyCommand())
	cmd.AddCommand(newModUploadCommand())
	cmd.AddCommand(newModListCommand())

	return cmd
}

func newModParseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "parse <file>",
		Short: "Parse and display a manifest file",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			m, err := ParseManifest(data)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(m)
		},
	}
}

func newModFmtCommand() *cobra.Command {
	var write bool

	cmd := &cobra.Command{
		Use:   "fmt <file>",
		Short: "Format a manifest file",
		Long: `Format and sort a manifest file. Writes to stdout by default.
Use -w to write the result back to the file in-place.

Formatting: sorting entries alphabetically, aligning columns.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			m, err := ParseManifest(data)
			if err != nil {
				return err
			}
			formatted := formatManifest(m)
			if write {
				return os.WriteFile(args[0], formatted, 0o644)
			}
			fmt.Print(string(formatted))
			return nil
		},
	}

	cmd.Flags().BoolVarP(&write, "write", "w", false, "write result to file instead of stdout")
	return cmd
}

func newModApplyCommand() *cobra.Command {
	var prefix string
	var dryRun bool
	var verbose bool

	cmd := &cobra.Command{
		Use:   "apply <file>",
		Short: "Build and download manifest entries to --prefix",
		Long: `Build and download all entries in a manifest file to --prefix directory,
preserving the output path structure.

Each entry is built via the remote /go/ endpoint (W9Y env var).
The manifest is used locally — no upload needed.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			m, err := ParseManifest(data)
			if err != nil {
				return err
			}

			host := strings.TrimRight(defaultHost, "/")
			client := &http.Client{
				Timeout: 5 * time.Minute,
			}

			var errs []error
			for _, entry := range m.Entries {
				entryVer := entry.Version
				if entryVer == "" {
					entryVer = m.Version
				}
				if entryVer == "" {
					entryVer = "latest"
				}

				u, err := url.JoinPath(host, "/go", entry.Source+"@"+entryVer)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: build URL: %w", entry.Output, err))
					continue
				}

				dest := filepath.Join(prefix, filepath.FromSlash(entry.Output))
				if dryRun {
					fmt.Printf("%s -> %s\n", u, dest)
					continue
				}

				ok, err := downloadBlob(client, u, dest)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", entry.Output, err))
					continue
				}
				if verbose {
					if ok {
						fmt.Println(entry.Output)
					} else {
						fmt.Printf("%s (cached)\n", entry.Output)
					}
				}
			}

			if len(errs) > 0 {
				for _, err := range errs {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
				}
				return fmt.Errorf("%d entries failed", len(errs))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&prefix, "prefix", "", "output directory (required)")
	cmd.MarkFlagRequired("prefix")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be downloaded")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-entry progress (cached/downloaded)")
	return cmd
}

func downloadBlob(client *http.Client, url, dest string) (downloaded bool, err error) {
	// Check if local file exists and compute its SHA-256
	var localSHA string
	if _, err := os.Stat(dest); err == nil {
		data, err := os.ReadFile(dest)
		if err == nil {
			localSHA = sha256Hex(data)
		}
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	if localSHA != "" {
		req.Header.Set("If-None-Match", `"`+localSHA+`"`)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return false, nil // already up to date, not downloaded
	}

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status %s", resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}

	f, err := os.Create(dest)
	if err != nil {
		return false, err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return false, err
	}
	return true, f.Close()
}

func formatManifest(m *Manifest) []byte {
	var buf bytes.Buffer

	// Header
	buf.WriteString("module ")
	buf.WriteString(m.Module)
	buf.WriteByte('\n')

	if m.GoVersion != "" || m.Version != "" {
		buf.WriteByte('\n')
	}

	if m.GoVersion != "" {
		buf.WriteString("go ")
		buf.WriteString(m.GoVersion)
		buf.WriteByte('\n')
	}

	if m.Version != "" {
		buf.WriteString("version ")
		buf.WriteString(m.Version)
		buf.WriteByte('\n')
	}

	if len(m.Entries) > 0 {
		buf.WriteByte('\n')
	}

	// Sort entries by output path
	sorted := make([]ManifestEntry, len(m.Entries))
	copy(sorted, m.Entries)
	slices.SortFunc(sorted, func(a, b ManifestEntry) int {
		return cmp.Compare(a.Output, b.Output)
	})

	// Find longest output path for alignment
	maxLen := 0
	for _, e := range sorted {
		if len(e.Output) > maxLen {
			maxLen = len(e.Output)
		}
	}

	for _, e := range sorted {
		buf.WriteString(e.Output)
		buf.WriteString(strings.Repeat(" ", maxLen-len(e.Output)+2))
		buf.WriteString(e.Source)
		if e.Version != "" && e.Version != m.Version {
			buf.WriteByte('@')
			buf.WriteString(e.Version)
		}
		buf.WriteByte('\n')
	}

	return buf.Bytes()
}

func newModUploadCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <file>",
		Short: "Upload a manifest to the server",
		Long: `Upload a manifest file to the server. The manifest is stored at
/manifest/<name>@<version> on the server.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			m, err := ParseManifest(data)
			if err != nil {
				return err
			}
			if err := uploadManifest(m, data); err != nil {
				return err
			}
			fmt.Println("ok")
			return nil
		},
	}
}

func newModListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List manifests from the server",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return listManifests()
		},
	}
}

func uploadManifest(m *Manifest, data []byte) error {
	host := strings.TrimRight(defaultHost, "/")

	ver := m.Version
	if ver == "" {
		ver = "latest"
	}

	u, err := url.JoinPath(host, "/api/manifest", m.Module, ver)
	if err != nil {
		return err
	}

	resp, err := httpPut(u, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload failed: %s", resp.Status)
	}

	return nil
}

func httpPut(u string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPut, u, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func listManifests() error {
	host := strings.TrimRight(defaultHost, "/")

	u, err := url.JoinPath(host, "/api/manifest")
	if err != nil {
		return err
	}

	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("list failed: %s", resp.Status)
	}

	var manifests map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&manifests); err != nil {
		return err
	}

	for name, versions := range manifests {
		for _, ver := range versions {
			fmt.Printf("%s@%s\n", name, ver)
		}
	}
	return nil
}

// ManifestStore stores manifests as text files under dataDir/manifests/.
type ManifestStore struct {
	dataDir string
}

func NewManifestStore(dataDir string) *ManifestStore {
	return &ManifestStore{dataDir: dataDir}
}

func (s *ManifestStore) manifestPath(name, version string) string {
	return filepath.Join(s.dataDir, "manifests", name, version)
}

func (s *ManifestStore) Get(name, version string) ([]byte, error) {
	return os.ReadFile(s.manifestPath(name, version))
}

func (s *ManifestStore) Set(name, version string, data []byte) error {
	p := s.manifestPath(name, version)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (s *ManifestStore) List() (map[string][]string, error) {
	dir := filepath.Join(s.dataDir, "manifests")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return make(map[string][]string), nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string][]string)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, v := range versions {
			if !v.IsDir() {
				result[e.Name()] = append(result[e.Name()], v.Name())
			}
		}
	}
	return result, nil
}

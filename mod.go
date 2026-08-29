package w9y

import (
	"bytes"
	"cmp"
	"context"
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
	"golang.org/x/mod/semver"
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
	cmd.AddCommand(newModListInstalledCommand())
	cmd.AddCommand(newModRemoveCommand())

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
			m, err := ParseManifest(args[0], data)
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
			m, err := ParseManifest(args[0], data)
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
	var files []string
	var progress string

	cmd := &cobra.Command{
		Use:   "apply [mod@ver ...]",
		Short: "Build and download manifest entries to --prefix",
		Long: `Build and download all entries in a manifest file to --prefix directory,
preserving the output path structure.

The manifest can be specified as:
  -f <file>        local manifest file (repeatable)
  <mod>@<ver>      remote manifest on the server
  <mod>            remote manifest, latest version (semver)

Multiple manifests can be given — all entries are downloaded to --prefix.
If no version is given, the latest semver is resolved automatically.
Each entry is built via the remote /go/ endpoint (W9Y env var).
`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if len(files) == 0 && len(args) == 0 {
				return fmt.Errorf("specify a manifest with -f <file> or <mod>@<ver>")
			}
			if prefix == "" {
				return fmt.Errorf("prefix required: set --prefix or $WANIX")
			}

			host := strings.TrimRight(defaultHost, "/")
			client := &http.Client{Timeout: 5 * time.Minute}

			// Collect all manifests
			var sources []manifestSource

			// Local files
			for _, f := range files {
				data, err := os.ReadFile(f)
				if err != nil {
					return err
				}
				sources = append(sources, manifestSource{data: data, label: f})
			}

			// Remote manifests
			for _, arg := range args {
				name, version, hasVersion := strings.Cut(arg, "@")
				if name == "" {
					return fmt.Errorf("empty module name in %q", arg)
				}

				fetchClient := &http.Client{Timeout: 30 * time.Second}

				if !hasVersion || version == "" {
					var err error
					version, err = resolveLatestManifestVersion(fetchClient, host, name)
					if err != nil {
						return fmt.Errorf("resolve latest version for %s: %w", name, err)
					}
					fmt.Fprintf(os.Stderr, "resolved %s@%s\n", name, version)
				}

				u, err := url.JoinPath(host, "/api/manifest", name, version)
				if err != nil {
					return err
				}
				resp, err := fetchClient.Get(u)
				if err != nil {
					return err
				}
				if resp.StatusCode != http.StatusOK {
					resp.Body.Close()
					return fmt.Errorf("fetch manifest %s@%s: %s", name, version, resp.Status)
				}
				data, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return err
				}
				sources = append(sources, manifestSource{data: data, label: name + "@" + version, ver: version})
			}

			// TUI vs plain output. The TUI is a Bubble Tea spinner+progress
			// view over the SAME download loop (see mod_tui.go); plain mode
			// prints exactly as before so headless callers (GearShell's w9y
			// orchestrator, scripts, CI) are unaffected. dry-run stays plain
			// even on a terminal - it only prints what would happen.
			ctx := context.Background()
			if !dryRun {
				switch progress {
				case "tui":
					return runApplyTUI(ctx, host, client, sources, prefix, verbose)
				case "plain":
					// fall through to the plain loop below
				default: // auto: TUI only on a real terminal
					if isTerminal() {
						return runApplyTUI(ctx, host, client, sources, prefix, verbose)
					}
				}
			}

			res := applySources(ctx, host, client, sources, prefix, dryRun, verbose, nil)
			if len(res.allErrors) > 0 {
				for _, err := range res.allErrors {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
				}
				return fmt.Errorf("%d entries failed", len(res.allErrors))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&prefix, "prefix", cmp.Or(os.Getenv("WANIX"), ""), "output directory (defaults to $WANIX)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be downloaded")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show per-entry progress (cached/downloaded)")
	cmd.Flags().StringArrayVarP(&files, "file", "f", nil, "local manifest file (repeatable)")
	cmd.Flags().StringVar(&progress, "progress", "auto", "apply output: auto (TUI on a terminal, plain otherwise), plain, tui")
	return cmd
}

// manifestSource is one input manifest: either a local file or a remote
// manifest resolved to a version.
type manifestSource struct {
	data  []byte
	label string
	ver   string // resolved version for remote manifests
}

// entryPhase marks the lifecycle of one download entry in the progress
// hook: entryStart (about to download), entryProgress (byte updates),
// entryDone (finished; ok carries success).
type entryPhase int

const (
	entryStart entryPhase = iota
	entryProgress
	entryDone
)

// applySourcesResult is everything the download loop produces: the
// per-mod registry records and the accumulated errors.
type applySourcesResult struct {
	recorded  map[string]map[string]*RegistryEntry
	versions  map[string]string
	allErrors []error
}

// applySources processes every manifest source: parse, download each
// entry, and persist the registry. Both the plain (headless) path and
// the TUI path run this same loop; the optional observe hook reports
// per-entry lifecycle + byte progress to the TUI renderer (nil in plain
// mode keeps the output byte-identical to before). ctx cancels the loop
// between entries and each in-flight request.
func applySources(ctx context.Context, host string, client *http.Client, sources []manifestSource, prefix string, dryRun, verbose bool, observe func(entry applyEntry, phase entryPhase, done, total int64, ok bool)) applySourcesResult {
	res := applySourcesResult{
		recorded: map[string]map[string]*RegistryEntry{},
		versions: map[string]string{},
	}
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			res.allErrors = append(res.allErrors, err)
			break
		}
		m, err := ParseManifest(src.label, src.data)
		if err != nil {
			res.allErrors = append(res.allErrors, fmt.Errorf("%s: parse: %w", src.label, err))
			continue
		}

		// Set manifest version from remote ref if not embedded
		if src.ver != "" && m.Version == "" {
			m.Version = src.ver
		}

		modName := m.Module
		if modName == "" {
			modName = strings.TrimSuffix(filepath.Base(src.label), filepath.Ext(src.label))
		}
		entries := map[string]*RegistryEntry{}

		if verbose {
			fmt.Fprintf(os.Stderr, "manifest: %s (%d entries)\n", src.label, len(m.Entries))
		}

		for _, entry := range m.Entries {
			if err := ctx.Err(); err != nil {
				res.allErrors = append(res.allErrors, err)
				break
			}
			entryVer := entry.Version
			if entryVer == "" {
				entryVer = m.Version
			}
			if entryVer == "" {
				entryVer = "latest"
			}

			u, err := url.JoinPath(host, "/go", entry.Source+"@"+entryVer)
			if err != nil {
				res.allErrors = append(res.allErrors, fmt.Errorf("%s: build URL: %w", entry.Output, err))
				if verbose {
					fmt.Printf("%s (error: %v)\n", entry.Output, err)
				}
				continue
			}

			dest := filepath.Join(prefix, filepath.FromSlash(entry.Output))
			if dryRun {
				fmt.Printf("%s -> %s\n", u, dest)
				continue
			}

			ae := applyEntry{Label: entry.Output, Mod: modName, URL: u, Dest: dest, Ver: entryVer}
			if observe != nil {
				observe(ae, entryStart, 0, 0, false)
			}

			ok, err := downloadBlob(ctx, client, u, dest, func(done, total int64) {
				if observe != nil {
					observe(ae, entryProgress, done, total, false)
				}
			})
			if err != nil {
				res.allErrors = append(res.allErrors, fmt.Errorf("%s: %w", entry.Output, err))
				if observe != nil {
					observe(ae, entryDone, 0, 0, false)
				}
				if verbose {
					fmt.Printf("%s (error: %v)\n", entry.Output, err)
				}
				continue
			}
			ae.Cached = !ok
			if fi, statErr := os.Stat(dest); statErr == nil {
				entries[entry.Output] = &RegistryEntry{
					Src:     u,
					Version: entryVer,
					Bytes:   fi.Size(),
				}
			}
			if observe != nil {
				observe(ae, entryDone, 0, 0, true)
			}
			if verbose {
				if ok {
					fmt.Println(entry.Output)
				} else {
					fmt.Printf("%s (cached)\n", entry.Output)
				}
			}
		}
		if !dryRun && len(entries) > 0 {
			res.recorded[modName] = entries
			res.versions[modName] = m.Version
		}
	}

	// Persist the registry for every successfully installed mod.
	if !dryRun && len(res.recorded) > 0 {
		reg, err := loadRegistry(prefix)
		if err != nil {
			res.allErrors = append(res.allErrors, err)
		} else {
			for name, entryMap := range res.recorded {
				recordMod(reg, name, res.versions[name], entryMap)
			}
			if err := saveRegistry(prefix, reg); err != nil {
				res.allErrors = append(res.allErrors, err)
			} else if verbose {
				fmt.Fprintf(os.Stderr, "registry: %s (%d mods)\n", registryPath(prefix), len(res.recorded))
			}
		}
	}
	return res
}

// applyEntry is one download the apply loop performs, shared with the
// TUI for rendering (and as the progress-hook argument).
type applyEntry struct {
	Label  string
	Mod    string
	URL    string
	Dest   string
	Ver    string
	Cached bool
}

// isTerminal reports whether stdout is a character device. A Stat-based
// check (no build tags) so it is safe under js/wasm, where Stat fails
// and headless (plain) output is always used.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// resolveLatestManifestVersion fetches the version list for a manifest name
// from the server and returns the latest semver.
func resolveLatestManifestVersion(client *http.Client, host, name string) (string, error) {
	u, err := url.JoinPath(host, "/api/manifest")
	if err != nil {
		return "", err
	}
	resp, err := client.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list manifests: %s", resp.Status)
	}

	var all map[string][]string
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return "", fmt.Errorf("decode manifest list: %w", err)
	}

	versions, ok := all[name]
	if !ok || len(versions) == 0 {
		return "", fmt.Errorf("no versions found for %s", name)
	}

	// Filter valid semvers and sort ascending
	var valid []string
	for _, v := range versions {
		if semver.IsValid(v) {
			valid = append(valid, v)
		}
	}
	if len(valid) == 0 {
		return "", fmt.Errorf("no valid semver versions found for %s", name)
	}
	semver.Sort(valid)
	return valid[len(valid)-1], nil
}

func downloadBlob(ctx context.Context, client *http.Client, url, dest string, progress func(done, total int64)) (downloaded bool, err error) {
	// If local file exists, check via HEAD whether it's still current.
	// The local file is raw wasm bytes (Go's HTTP transport auto-decompresses
	// Content-Encoding: gzip). The server's ETag is the raw wasm SHA.
	var localSHA string
	if _, err := os.Stat(dest); err == nil {
		data, err := os.ReadFile(dest)
		if err == nil {
			localSHA = sha256Hex(data)
		}

		// HEAD request to get server's ETag — only when we already have
		// a local copy to compare against.
		headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err != nil {
			return false, fmt.Errorf("create HEAD request: %w", err)
		}
		headResp, err := client.Do(headReq)
		if err != nil {
			return false, fmt.Errorf("HEAD: %w", err)
		}
		headResp.Body.Close()

		serverSHA := strings.Trim(headResp.Header.Get("Etag"), `"`)
		if serverSHA != "" && localSHA == serverSHA {
			return false, nil // up to date
		}
	}

	// Download
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("status %s", resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return false, err
	}
	defer f.Close()

	body := resp.Body
	if progress != nil {
		body = &progressReader{r: resp.Body, total: resp.ContentLength, fn: progress}
	}
	if _, err := io.Copy(f, body); err != nil {
		return false, err
	}
	if progress != nil {
		progress(resp.ContentLength, resp.ContentLength)
	}
	return true, f.Close()
}

// progressReader wraps an http response body and reports byte progress
// through a callback (used by the apply TUI's progress bars). It is a
// no-op when the callback is nil, which keeps the plain path identical.
type progressReader struct {
	r     io.ReadCloser
	total int64
	fn    func(done, total int64)
	n     int64
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.n += int64(n)
	if p.fn != nil && n > 0 {
		p.fn(p.n, p.total)
	}
	return n, err
}

func (p *progressReader) Close() error {
	return p.r.Close()
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
			m, err := ParseManifest(args[0], data)
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

func newModListInstalledCommand() *cobra.Command {
	var prefix string

	cmd := &cobra.Command{
		Use:   "list-installed",
		Short: "List mods installed by w9y mod apply (reads w9y-registry.json)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if prefix == "" {
				return fmt.Errorf("prefix required: set --prefix or $WANIX")
			}
			reg, err := loadRegistry(prefix)
			if err != nil {
				return err
			}
			if len(reg.Mods) == 0 {
				fmt.Println("no mods installed")
				return nil
			}
			fmt.Printf("%-24s %-12s %-8s %s\n", "MOD", "VERSION", "ENTRIES", "INSTALLED_AT")
			for name, rec := range reg.Mods {
				fmt.Printf("%-24s %-12s %-8d %s\n", name, rec.Version, len(rec.Entries), rec.InstalledAt)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&prefix, "prefix", cmp.Or(os.Getenv("WANIX"), ""), "output directory (defaults to $WANIX)")
	return cmd
}

func newModRemoveCommand() *cobra.Command {
	var prefix string

	cmd := &cobra.Command{
		Use:   "remove <mod> [mod ...]",
		Short: "Remove installed mods (files + registry records)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if prefix == "" {
				return fmt.Errorf("prefix required: set --prefix or $WANIX")
			}
			reg, err := loadRegistry(prefix)
			if err != nil {
				return err
			}
			var allErrors []error
			for _, name := range args {
				rec, ok := reg.Mods[name]
				if !ok {
					fmt.Fprintf(os.Stderr, "mod %s: not installed\n", name)
					continue
				}
				if err := deleteModFiles(prefix, rec); err != nil {
					allErrors = append(allErrors, fmt.Errorf("%s: %w", name, err))
					continue
				}
				delete(reg.Mods, name)
				fmt.Printf("removed %s (%d files)\n", name, len(rec.Entries))
			}
			if err := saveRegistry(prefix, reg); err != nil {
				allErrors = append(allErrors, err)
			}
			if len(allErrors) > 0 {
				for _, err := range allErrors {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
				}
				return fmt.Errorf("%d errors", len(allErrors))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&prefix, "prefix", cmp.Or(os.Getenv("WANIX"), ""), "output directory (defaults to $WANIX)")
	return cmd
}

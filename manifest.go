package w9y

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/modfile"
)

// Manifest describes a named group of WASM output files mapped to Go import
// paths, using a go.mod-like format.
type Manifest struct {
	Module    string          // manifest name (e.g. "go")
	GoVersion string          // Go toolchain version (optional)
	Version   string          // shared default version (optional)
	Entries   []ManifestEntry
}

// ManifestEntry maps a single output path to a Go import path with an
// optional per-entry version override.
type ManifestEntry struct {
	Output  string // e.g. "bin/go"
	Source  string // e.g. "github.com/golang/go/src/cmd/go"
	Version string // per-entry version override (empty = use manifest default or @latest)
}

// ParseManifest parses a go.mod-like manifest from data.
// It uses golang.org/x/mod/modfile.ParseLax for lexing, then interprets
// custom directives:
//
//	module <name>       — manifest name (required)
//	go <version>        — Go toolchain version (optional)
//	version <semver>    — shared default version (optional)
//	<output> <import>   — entry line
//	<output> <import>@<ver> — entry with per-entry version override
func ParseManifest(data []byte) (*Manifest, error) {
	f, err := modfile.ParseLax("manifest.mod", data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	m := &Manifest{}
	if f.Module != nil {
		m.Module = f.Module.Mod.Path
	}

	if f.Go != nil {
		m.GoVersion = f.Go.Version
	}

	for _, stmt := range f.Syntax.Stmt {
		line, ok := stmt.(*modfile.Line)
		if !ok || len(line.Token) == 0 {
			continue
		}

		switch line.Token[0] {
		case "module", "go":
			// already handled by modfile.Parse
		case "version":
			if len(line.Token) < 2 {
				return nil, errors.New("version directive requires an argument")
			}
			m.Version = line.Token[1]
		default:
			// entry: <output-path> <import-path>[@<version>]
			if len(line.Token) < 2 {
				return nil, fmt.Errorf("unknown directive %q", line.Token[0])
			}
			source, ver, _ := strings.Cut(line.Token[1], "@")
			if ver == "" {
				ver = m.Version // inherit manifest default
			}
			m.Entries = append(m.Entries, ManifestEntry{
				Output:  line.Token[0],
				Source:  source,
				Version: ver,
			})
		}
	}

	if m.Module == "" {
		return nil, errors.New("module directive is required")
	}

	return m, nil
}



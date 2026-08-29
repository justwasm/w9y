// Registry of installed mods, owned by w9y itself and stored at the
// install prefix root (w9y-registry.json). w9y mod apply writes it after
// a successful install; mod list-installed reads it; mod remove deletes
// the recorded files and the record. GearShell and other tools may read
// it but must not write it - w9y is the single bookkeeper.

package w9y

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RegistryFileName sits at the prefix root, next to the installed tree.
const RegistryFileName = "w9y-registry.json"

// RegistryEntry is one installed output of a mod.
type RegistryEntry struct {
	Src     string `json:"src"`     // download URL used
	Version string `json:"version"` // resolved entry version
	Bytes   int64  `json:"bytes"`   // file size on disk
}

// ModRecord is one installed mod.
type ModRecord struct {
	Version     string                   `json:"version"`
	InstalledAt string                   `json:"installedAt"`
	Entries     map[string]*RegistryEntry `json:"entries"`
}

// Registry is the whole file: versioned envelope + mods by name.
type Registry struct {
	Version int                   `json:"version"`
	Mods    map[string]*ModRecord `json:"mods"`
}

func newRegistry() *Registry {
	return &Registry{Version: 1, Mods: map[string]*ModRecord{}}
}

func registryPath(prefix string) string {
	return filepath.Join(prefix, RegistryFileName)
}

func loadRegistry(prefix string) (*Registry, error) {
	data, err := os.ReadFile(registryPath(prefix))
	if err != nil {
		if os.IsNotExist(err) {
			return newRegistry(), nil
		}
		return nil, fmt.Errorf("read registry: %w", err)
	}
	reg := newRegistry()
	if err := json.Unmarshal(data, reg); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", registryPath(prefix), err)
	}
	if reg.Version == 0 {
		reg.Version = 1
	}
	if reg.Mods == nil {
		reg.Mods = map[string]*ModRecord{}
	}
	return reg, nil
}

func saveRegistry(prefix string, reg *Registry) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	// Replace atomically so a crash never leaves a half-written file.
	tmp := registryPath(prefix) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return os.Rename(tmp, registryPath(prefix))
}

// recordMod replaces (or creates) the registry record for one mod with
// the entries collected during an apply. `installedAt` refreshes on every
// apply so the record reflects the last install time.
func recordMod(reg *Registry, name, version string, entries map[string]*RegistryEntry) {
	reg.Mods[name] = &ModRecord{
		Version:     version,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
		Entries:     entries,
	}
}

// deleteModFiles removes every recorded file of a mod under the prefix.
func deleteModFiles(prefix string, rec *ModRecord) error {
	var firstErr error
	for output := range rec.Entries {
		dest := filepath.Join(prefix, filepath.FromSlash(output))
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("remove %s: %w", dest, err)
		}
	}
	return firstErr
}

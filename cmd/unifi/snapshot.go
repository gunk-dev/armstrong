package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// writeSnapshot saves the live site, exactly as `unifi export` would print it,
// to a new timestamped file in dir, then deletes all but the newest keep
// snapshots (keep <= 0 keeps every one). It returns the new file's path.
//
// A snapshot never holds a passphrase: the export shape has no field for one.
// Each SSID carries a passphraseEnv instead — the one the instance file uses
// for it where the instance file declares that SSID, so `unifi restore` reads
// the same environment variable `sync` does, and export's generated name
// otherwise.
func writeSnapshot(c *client, siteID string, want site, dir string, keep int, warn io.Writer) (string, error) {
	doc, err := buildExport(c, siteID, warn)
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	envs := map[string]string{}
	for _, w := range want.WiFi {
		envs[w.Name] = w.Security.PassphraseEnv
	}
	for i, w := range doc.WiFi {
		if env := envs[w.Name]; env != "" && w.Security.PassphraseEnv != "" {
			doc.WiFi[i].Security.PassphraseEnv = env
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	// The timestamp sorts lexically in time order, which is what pruning
	// relies on.
	name := "snapshot-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".json"
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return "", fmt.Errorf("snapshot: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}

	if keep > 0 {
		if err := pruneSnapshots(dir, keep); err != nil {
			return "", err
		}
	}
	return path, nil
}

func pruneSnapshots(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "snapshot-") && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return fmt.Errorf("snapshot: remove old snapshot: %w", err)
		}
		names = names[1:]
	}
	return nil
}

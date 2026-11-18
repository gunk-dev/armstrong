package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func (a *attempt) cue(ctx context.Context, label string, capture bool, args ...string) ([]byte, error) {
	return a.r.run(ctx, childSpec{
		label: label, tool: "cue", args: args, dir: a.repo, capture: capture,
	})
}

// prepareDNS vets every zone, then exports them all into zones.json. Each
// immediate directory of dns/ is a zone package, taken in sorted order and
// named with a trailing slash so cue never reads a dotted name like gunk.dev
// as a file; with no directories, dns/ itself is the one zone.
func (a *attempt) prepareDNS(ctx context.Context) result {
	if link, err := escapingSymlink(a.repo); err != nil {
		return failed("read checkout: %v", err)
	} else if link != "" {
		return failed("%s is a symlink out of the commit", link)
	}
	entries, err := os.ReadDir(filepath.Join(a.repo, "dns"))
	if err != nil {
		return failed("the infra commit has no dns directory")
	}
	var packages []string
	for _, e := range entries { // os.ReadDir sorts by name
		if e.Type()&fs.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(a.repo, "dns", e.Name())); err == nil && st.IsDir() {
				return failed("dns/%s links to a directory; commit the zone directory itself", e.Name())
			}
		}
		if e.IsDir() {
			packages = append(packages, "./dns/"+e.Name()+"/")
		}
	}
	perZone := len(packages) > 0
	if !perZone {
		packages = []string{"./dns"}
	}

	for _, p := range packages {
		if _, err := a.cue(ctx, "cue vet "+p, false, "vet", p); err != nil {
			return childResult(err, false)
		}
	}
	var exports []json.RawMessage
	for _, p := range packages {
		out, err := a.cue(ctx, "cue export "+p, true, "export", p, "--out", "json")
		if err != nil {
			return childResult(err, false)
		}
		// dns sync --prune reads a zone without records as an empty zone and
		// deletes everything it may prune, so an empty zone must say so.
		var zone struct {
			Records json.RawMessage `json:"records"`
		}
		if err := json.Unmarshal(out, &zone); err != nil {
			return failed("cue export %s is not one zone object: %v", p, err)
		}
		if !bytes.HasPrefix(bytes.TrimSpace(zone.Records), []byte("[")) {
			return failed("cue export %s has no records list; write records: [] for an empty zone", p)
		}
		exports = append(exports, out)
	}

	// One zone object for the flat layout, an array of them per zone
	// directory: the two inputs dns sync accepts.
	var zones []byte
	if perZone {
		zones, err = json.Marshal(exports)
	} else {
		var buf bytes.Buffer
		err = json.Compact(&buf, exports[0])
		zones = buf.Bytes()
	}
	if err != nil {
		return failed("aggregate zones: %v", err)
	}
	a.zonesFile = filepath.Join(a.work, "zones.json")
	if err := os.WriteFile(a.zonesFile, zones, 0o600); err != nil {
		return failed("write zones.json: %v", err)
	}
	a.rec.ConfigSHA256 = map[string]string{"zones.json": sha256Hex(zones)}
	return completed()
}

// escapingSymlink returns the first committed symlink in the checkout that
// resolves outside it: through one, cue could read zones or imported packages
// that are not part of the commit. Each link is resolved fully from where it
// lives, so a path through it can lead nowhere its resolution does not; a
// dangling link leads nowhere at all.
func escapingSymlink(repo string) (string, error) {
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", err
	}
	var found string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == ".git" && filepath.Dir(p) == root:
			return filepath.SkipDir
		case d.Type()&fs.ModeSymlink == 0:
			return nil
		}
		target, err := filepath.EvalSymlinks(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if t, err := filepath.Rel(root, target); err != nil || t == ".." || strings.HasPrefix(t, "../") {
			found, _ = filepath.Rel(root, p)
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

func (a *attempt) dnsSync(ctx context.Context, label string, mutator, capture bool, args ...string) ([]byte, error) {
	return a.r.run(ctx, childSpec{
		label: label, tool: "dns", args: args, dir: a.work,
		env: []string{
			"PORKBUN_API_KEY=" + a.secrets.porkbunKey,
			"PORKBUN_SECRET_KEY=" + a.secrets.porkbunSecret,
		},
		stdin: a.zonesFile, mutator: mutator, capture: capture,
	})
}

// planDNS is a dry run's inspect: dns sync reads the live records and prints
// its plan without writing, and the plan is the one child output shown.
func (a *attempt) planDNS(ctx context.Context) result {
	out, err := a.dnsSync(ctx, "dns sync --dry-run", false, true, "sync", "--prune", "--dry-run")
	if err != nil {
		return childResult(err, false)
	}
	if _, err := io.WriteString(a.stdout, a.redact(string(out))); err != nil {
		return failed("print plan: %v", err)
	}
	return completed()
}

// dnsNoWrite is the status dns sync exits with when it failed before
// attempting any write, so the records are unchanged.
const dnsNoWrite = 3

// applyDNS runs the real sync. Its exit status alone decides the outcome; its
// announcements are never parsed.
func (a *attempt) applyDNS(ctx context.Context) result {
	_, err := a.dnsSync(ctx, "dns sync", true, false, "sync", "--prune")
	var ce *childErr
	if errors.As(err, &ce) && ce.kind == childExited && ce.code == dnsNoWrite {
		return failed("%v: it failed before attempting any write", err)
	}
	return childResult(err, true)
}

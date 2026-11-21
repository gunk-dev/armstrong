package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

var attemptIDRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

type convergeEntry struct {
	target     string
	status     string
	detail     string
	recordPath string
}

// runConverge discovers the apps exposed by the infra flake at req.rev, runs
// dns-sync first and then each app in sorted order under sess's lock, stops on
// the first failed or uncertain attempt, and prints a summary of every target.
func runConverge(ctx context.Context, sess *session, req request) int {
	red := newRedactor(sess.secrets.flyToken, sess.secrets.porkbunKey, sess.secrets.porkbunSecret)

	apps, err := discoverApps(ctx, sess, req, red)
	if err != nil {
		fmt.Fprintf(sess.stderr, "deploy: discover apps: %s\n", red.text(err.Error()))
		return exitFailure
	}

	entries := make([]convergeEntry, 0, 1+len(apps))
	entries = append(entries, convergeEntry{target: verbDNS, status: "not attempted"})
	for _, name := range apps {
		entries = append(entries, convergeEntry{target: "app " + name, status: "not attempted"})
	}

	failedAny := false

	// 1. dns-sync
	if ctx.Err() != nil {
		failedAny = true
	} else {
		fmt.Fprintf(sess.stderr, "deploy: converging %s\n", verbDNS)
		dnsReq := request{
			verb:                verbDNS,
			dryRun:              req.dryRun,
			infra:               req.infra,
			rev:                 req.rev,
			requireMainAncestor: req.requireMainAncestor,
			stateDir:            req.stateDir,
			porkbunKeyFile:      req.porkbunKeyFile,
			porkbunSecretFile:   req.porkbunSecretFile,
		}
		a, err := runAttempt(ctx, sess, dnsReq, false)
		if err != nil {
			fmt.Fprintf(sess.stderr, "deploy: create record: %s\n", red.text(err.Error()))
			entries[0].status = outcomeFailed
			entries[0].detail = "create record failed"
			failedAny = true
		} else {
			entries[0].recordPath = filepath.Join(a.runDir, "record.json")
			entries[0].status = a.rec.Outcome
			if req.dryRun && a.rec.Outcome == outcomeCompleted {
				entries[0].detail = "dry run"
			}
			if a.recordFailed {
				entries[0].status = outcomeFailed
				entries[0].detail = "recording failed"
				failedAny = true
			} else if a.rec.Outcome != outcomeCompleted {
				failedAny = true
			}
		}
	}

	// 2. Apps in sorted order.
	for i, name := range apps {
		if failedAny || ctx.Err() != nil {
			failedAny = true
			break
		}
		idx := i + 1
		fmt.Fprintf(sess.stderr, "deploy: converging app %s\n", name)
		appReq := request{
			verb:                verbApp,
			name:                name,
			dryRun:              req.dryRun,
			infra:               req.infra,
			rev:                 req.rev,
			requireMainAncestor: req.requireMainAncestor,
			stateDir:            req.stateDir,
			flyTokenFile:        req.flyTokenFile,
		}
		a, err := runAttempt(ctx, sess, appReq, true)
		if err != nil {
			fmt.Fprintf(sess.stderr, "deploy: create record: %s\n", red.text(err.Error()))
			entries[idx].status = outcomeFailed
			entries[idx].detail = "create record failed"
			failedAny = true
			break
		}
		entries[idx].recordPath = filepath.Join(a.runDir, "record.json")
		switch {
		case a.recordFailed:
			entries[idx].status = outcomeFailed
			entries[idx].detail = "recording failed"
		case req.dryRun && a.rec.Outcome == outcomeSkipped:
			entries[idx].status = "would skip"
			entries[idx].detail = a.skipAfterInspect
		case req.dryRun && a.rec.Outcome == outcomeCompleted:
			entries[idx].status = "would deploy"
			entries[idx].detail = a.deployReason
		case a.rec.Outcome == outcomeSkipped:
			entries[idx].status = outcomeSkipped
			entries[idx].detail = a.skipAfterInspect
		default:
			entries[idx].status = a.rec.Outcome
		}
		if a.recordFailed || (a.rec.Outcome != outcomeCompleted && a.rec.Outcome != outcomeSkipped) {
			failedAny = true
			break
		}
	}

	if err := writeSummary(sess.stdout, entries, red); err != nil {
		fmt.Fprintf(sess.stderr, "deploy: write summary: %s\n", red.text(err.Error()))
		return exitFailure
	}
	if failedAny || ctx.Err() != nil {
		return exitFailure
	}
	return exitOK
}

func writeSummary(w io.Writer, entries []convergeEntry, red *redactor) error {
	if _, err := fmt.Fprintln(w, "summary:"); err != nil {
		return err
	}
	for _, e := range entries {
		var b strings.Builder
		b.WriteString("  ")
		b.WriteString(e.target)
		b.WriteString(": ")
		b.WriteString(e.status)
		if e.detail != "" {
			b.WriteString(" (")
			b.WriteString(red.text(e.detail))
			b.WriteString(")")
		}
		if e.recordPath != "" {
			b.WriteByte(' ')
			b.WriteString(e.recordPath)
		}
		if _, err := fmt.Fprintln(w, b.String()); err != nil {
			return err
		}
	}
	return nil
}

// discoverApps fetches req.rev into a private temporary directory under
// sess.stateDir, lists packages.<system> from the flake, builds each app's
// deploy-<name>-config output to resolve its Fly app name, refuses unpaired
// outputs, invalid names, or two outputs targeting the same Fly app, checks
// that flake.lock did not change, and returns the sorted app names.
func discoverApps(ctx context.Context, sess *session, req request, red *redactor) ([]string, error) {
	work, err := os.MkdirTemp(sess.stateDir, ".discover-*")
	if err != nil {
		return nil, fmt.Errorf("create discovery directory: %w", err)
	}
	defer removeTree(work)

	repo := filepath.Join(work, "src")
	home := filepath.Join(work, "home")
	runtimeDir := filepath.Join(work, "run")
	tmp := filepath.Join(work, "tmp")
	links := filepath.Join(work, "links")
	for _, d := range []string{repo, home, runtimeDir, tmp, links} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, fmt.Errorf("create discovery directory: %w", err)
		}
	}

	r := &runner{
		tools:   sess.tools,
		path:    toolPath,
		home:    home,
		runtime: runtimeDir,
		tmp:     tmp,
		lock:    sess.lock,
		grace:   killGrace,
		gate:    sess.gate,
		red:     red,
		stderr:  sess.stderr,
	}

	_, lockSHA, err := checkoutCommit(ctx, r, repo, req.infra, req.rev, req.requireMainAncestor)
	if err != nil {
		return nil, err
	}

	flake := (&url.URL{Scheme: "git+file", Path: repo, RawQuery: "rev=" + req.rev}).String()
	applyExpr := fmt.Sprintf(`f: builtins.attrNames ((f.packages or {}).%q or {})`, sess.system)
	out, err := r.run(ctx, childSpec{
		label: "nix eval packages",
		tool:  "nix",
		args: []string{
			"eval", "--extra-experimental-features", "nix-command flakes",
			"--no-update-lock-file", "--no-write-lock-file",
			"--option", "accept-flake-config", "false",
			"--json", "--apply", applyExpr,
			flake + "#.",
		},
		dir:     work,
		capture: true,
	})
	if err != nil {
		return nil, err
	}
	if err := checkLockFile(repo, lockSHA); err != nil {
		return nil, err
	}
	var attrs []string
	if err := json.Unmarshal(out, &attrs); err != nil {
		return nil, fmt.Errorf("parse flake package names: %w", err)
	}
	names, err := discoverAppNames(attrs)
	if err != nil {
		return nil, err
	}
	seenFlyApps := make(map[string]string, len(names))
	for _, name := range names {
		configAttr := "deploy-" + name + "-config"
		cfg, err := nixBuildAttr(ctx, r, work, repo, req.rev, sess.system, configAttr)
		if err != nil {
			return nil, err
		}
		fly, err := readFlyConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", configAttr, err)
		}
		if prev, dup := seenFlyApps[fly.app]; dup {
			return nil, fmt.Errorf("packages %s and %s both target Fly app %q", prev, configAttr, fly.app)
		}
		seenFlyApps[fly.app] = configAttr
	}
	if len(names) > 0 {
		if err := checkLockFile(repo, lockSHA); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// discoverAppNames extracts app names from packages.<system> attribute names
// matching deploy-<name>-image and deploy-<name>-config, refusing any unpaired
// output or invalid app name.
func discoverAppNames(attrs []string) ([]string, error) {
	hasImage := map[string]bool{}
	hasConfig := map[string]bool{}
	for _, attr := range attrs {
		rest, ok := strings.CutPrefix(attr, "deploy-")
		if !ok {
			continue
		}
		if name, ok := strings.CutSuffix(rest, "-image"); ok {
			if !nameRE.MatchString(name) {
				return nil, fmt.Errorf("flake package %q has invalid app name %q", attr, name)
			}
			hasImage[name] = true
		} else if name, ok := strings.CutSuffix(rest, "-config"); ok {
			if !nameRE.MatchString(name) {
				return nil, fmt.Errorf("flake package %q has invalid app name %q", attr, name)
			}
			hasConfig[name] = true
		}
	}

	all := map[string]bool{}
	for name := range hasImage {
		all[name] = true
	}
	for name := range hasConfig {
		all[name] = true
	}
	names := slices.Sorted(maps.Keys(all))
	for _, name := range names {
		if !hasConfig[name] {
			return nil, fmt.Errorf("flake exposes deploy-%s-image without deploy-%s-config", name, name)
		}
		if !hasImage[name] {
			return nil, fmt.Errorf("flake exposes deploy-%s-config without deploy-%s-image", name, name)
		}
	}
	return names, nil
}

// writeAppPointer atomically updates DEPLOY_STATE_DIR/apps/<fly-app> under the
// state directory lock to name this attempt's record.
func (a *attempt) writeAppPointer() error {
	if !appRE.MatchString(a.app) {
		return fmt.Errorf("invalid Fly app name %q", a.app)
	}
	appsDir := filepath.Join(a.req.stateDir, "apps")
	if err := os.Mkdir(appsDir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := checkPrivateDir(appsDir); err != nil {
		return err
	}
	if err := syncDir(a.req.stateDir); err != nil {
		return err
	}
	return writeAtomic(appsDir, a.app, []byte(a.rec.AttemptID+"\n"))
}

// canSkipApp decides whether this app can be skipped. Fly translates fly.json
// into per-Machine API configs with defaults and fields that cannot be
// compared back reliably, so skipping requires both:
//   - the per-app pointer file DEPLOY_STATE_DIR/apps/<fly-app> names a
//     completed non-dry-run deploy record with the exact same flake name, Fly
//     app, platform, fly.json SHA-256 and image digest; and
//   - the live Machines observed by inspect all settle on that image digest and
//     satisfy fly.json's started-Machine rule.
//
// Any missing, unreadable, malformed or dangling pointer falls back to
// deploying.
func (a *attempt) canSkipApp() (bool, string) {
	appsDir := filepath.Join(a.req.stateDir, "apps")
	ptrPath := filepath.Join(appsDir, a.app)
	f, err := os.OpenFile(ptrPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "no previous mutating attempt recorded for this app"
	}
	if err != nil {
		return false, fmt.Sprintf("cannot open app pointer: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, fmt.Sprintf("cannot inspect app pointer: %v", err)
	}
	if !st.Mode().IsRegular() {
		return false, "app pointer is not a regular file"
	}
	if err := checkPrivateDir(appsDir); err != nil {
		return false, fmt.Sprintf("app pointer directory is unsafe: %v", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return false, fmt.Sprintf("cannot read app pointer: %v", err)
	}
	if len(data) > 256 {
		return false, "app pointer is malformed"
	}
	id := strings.TrimSuffix(string(data), "\n")
	if !attemptIDRE.MatchString(id) {
		return false, "app pointer does not name a valid attempt"
	}
	if id == a.rec.AttemptID {
		return false, "app pointer names the current attempt"
	}

	recPath := filepath.Join(a.req.stateDir, "runs", id, "record.json")
	recData, err := os.ReadFile(recPath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Sprintf("app pointer names missing record %s", id)
	}
	if err != nil {
		return false, fmt.Sprintf("cannot read record %s: %v", id, err)
	}
	var latest record
	if err := json.Unmarshal(recData, &latest); err != nil {
		return false, fmt.Sprintf("cannot parse record %s: %v", id, err)
	}
	if latest.SchemaVersion != 1 {
		return false, fmt.Sprintf("unsupported schema version %d in record %s", latest.SchemaVersion, id)
	}
	if latest.AttemptID != id {
		return false, fmt.Sprintf("record %s has mismatched attempt_id %q", id, latest.AttemptID)
	}
	if latest.Verb != verbApp || latest.DryRun {
		return false, fmt.Sprintf("record %s is not a non-dry-run app deploy", id)
	}
	if latest.Name == nil {
		return false, fmt.Sprintf("record %s has no app name", id)
	}
	if latest.Outcome != outcomeCompleted {
		return false, fmt.Sprintf("last mutating attempt %s ended %s", latest.AttemptID, latest.Outcome)
	}
	if *latest.Name != a.req.name {
		return false, fmt.Sprintf("last mutating attempt %s was for flake app %q, want %q", latest.AttemptID, *latest.Name, a.req.name)
	}
	if latest.Platform != a.system {
		return false, fmt.Sprintf("last mutating attempt %s was for platform %s, want %s", latest.AttemptID, latest.Platform, a.system)
	}
	if latest.TargetsAfter == nil || latest.TargetsAfter.App != a.app {
		return false, fmt.Sprintf("last mutating attempt %s targeted a different Fly app", latest.AttemptID)
	}
	if latest.ConfigSHA256["fly.json"] == "" || latest.ConfigSHA256["fly.json"] != a.rec.ConfigSHA256["fly.json"] {
		return false, fmt.Sprintf("fly.json changed since attempt %s", latest.AttemptID)
	}
	if latest.ImageDigest == nil || *latest.ImageDigest != a.digest {
		return false, fmt.Sprintf("image digest changed since attempt %s", latest.AttemptID)
	}
	if why, _ := judge(a.rec.TargetsBefore, a.rec.TargetsBefore, a.digest, a.requireStarted); why != "" {
		return false, fmt.Sprintf("live app does not match target state: %s", why)
	}
	return true, fmt.Sprintf("live Machines match image digest and last deploy's fly.json hash (attempt %s)", latest.AttemptID)
}

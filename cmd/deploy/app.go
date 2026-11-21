package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const registry = "registry.fly.io"

var appRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// nixBuildAttr builds packages.<system>.<attr> from the checkout at rev and
// returns the output's store path. The out-link is an execution-only GC root
// inside work/links, removed when work is cleaned up.
func nixBuildAttr(ctx context.Context, r *runner, work, repo, rev, system, attr string) (string, error) {
	flake := (&url.URL{Scheme: "git+file", Path: repo, RawQuery: "rev=" + rev}).String()
	link := filepath.Join(work, "links", attr)
	_, err := r.run(ctx, childSpec{
		label: "nix build " + attr,
		tool:  "nix",
		// The private HOME hides any user nix.conf, so the features this
		// needs are enabled here rather than assumed.
		args: []string{
			"build", "--extra-experimental-features", "nix-command flakes",
			"--no-update-lock-file", "--no-write-lock-file",
			"--option", "accept-flake-config", "false",
			"--out-link", link,
			flake + "#packages." + system + "." + attr,
		},
		dir: work,
	})
	if err != nil {
		return "", err
	}
	out, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", fmt.Errorf("nix build %s left no output link: %w", attr, err)
	}
	if err := checkInStore(out); err != nil {
		return "", fmt.Errorf("nix build %s: %w", attr, err)
	}
	return out, nil
}

// nixBuild builds packages.<system>.<attr> from the attempt's checkout.
func (a *attempt) nixBuild(ctx context.Context, attr string) (string, error) {
	return nixBuildAttr(ctx, a.r, a.work, a.repo, a.req.rev, a.system, attr)
}

// checkInStore refuses a resolved path outside the Nix store: an output that
// links to a mutable host path could change after it was checked.
func checkInStore(resolved string) error {
	store, err := filepath.EvalSymlinks(storeDir)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(store, resolved); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("output resolves to %s, outside the Nix store", resolved)
	}
	return nil
}

// prepareApp builds the app's config and checked image and validates both.
func (a *attempt) prepareApp(ctx context.Context) result {
	configAttr := "deploy-" + a.req.name + "-config"
	cfg, err := a.nixBuild(ctx, configAttr)
	if err != nil {
		return childResult(err, false)
	}
	fly, err := readFlyConfig(cfg)
	if err != nil {
		return failed("%s: %v", configAttr, err)
	}
	a.app, a.configFile, a.requireStarted = fly.app, fly.file, fly.requireStarted
	a.rec.ConfigSHA256 = map[string]string{"fly.json": fly.sum}

	imageAttr := "deploy-" + a.req.name + "-image"
	img, err := a.nixBuild(ctx, imageAttr)
	if err != nil {
		return childResult(err, false)
	}
	info, err := inspectArchive(img)
	if err != nil {
		return failed("%s: %v", imageAttr, err)
	}
	if err := checkPlatform(info, a.system); err != nil {
		return failed("%s: %v", imageAttr, err)
	}
	a.imagePath, a.digest = img, info.digest
	a.rec.ImageDigest = &a.digest

	if err := a.checkLock(); err != nil {
		return failed("%v", err)
	}
	return completed()
}

// flyConfig is what deploy reads from fly.json.
type flyConfig struct {
	app  string // the Fly app, the deploy's only target
	file string // fly.json in the store
	sum  string // SHA-256 of the bytes everything here was read from
	// requireStarted: the configuration keeps a Machine running, so
	// verification requires one started.
	requireStarted bool
}

// flyService holds the keys of http_service and each services entry that
// decide whether every Machine may stop.
type flyService struct {
	AutoStop   json.RawMessage `json:"auto_stop_machines"`
	MinRunning *int            `json:"min_machines_running"`
	Processes  json.RawMessage `json:"processes"`
}

// keepsRunning reports whether the service keeps a Machine running: a
// positive min_machines_running, or auto-stop absent or off. Auto-stop is on
// as "stop", "suspend" or true.
func (s flyService) keepsRunning() bool {
	if s.MinRunning != nil && *s.MinRunning > 0 {
		return true
	}
	var autoStop any
	if json.Unmarshal(s.AutoStop, &autoStop) != nil {
		return true
	}
	return autoStop != "stop" && autoStop != "suspend" && autoStop != true
}

// readFlyConfig accepts a directory holding only fly.json, whose "app" names
// the Fly app, and reads it once: the name, the services' auto-stop keys and
// the hash all come from the same bytes.
func readFlyConfig(dir string) (flyConfig, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return flyConfig{}, fmt.Errorf("config output is not a directory: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != "fly.json" || !entries[0].Type().IsRegular() {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return flyConfig{}, fmt.Errorf("config output must hold only fly.json, found %v", names)
	}
	file, err := filepath.EvalSymlinks(filepath.Join(dir, "fly.json"))
	if err != nil {
		return flyConfig{}, err
	}
	if err := checkInStore(file); err != nil {
		return flyConfig{}, fmt.Errorf("fly.json: %w", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return flyConfig{}, err
	}
	var cfg struct {
		App         string          `json:"app"`
		HTTPService *flyService     `json:"http_service"`
		Services    []flyService    `json:"services"`
		Processes   json.RawMessage `json:"processes"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return flyConfig{}, fmt.Errorf("fly.json: %w", err)
	}
	if !appRE.MatchString(cfg.App) {
		return flyConfig{}, fmt.Errorf("fly.json app %q is not a Fly app name", cfg.App)
	}
	services := cfg.Services
	if cfg.HTTPService != nil {
		services = append(services, *cfg.HTTPService)
	}
	return flyConfig{app: cfg.App, file: file, sum: sha256Hex(data), requireStarted: requireStarted(services, cfg.Processes)}, nil
}

// requireStarted reports whether the configuration keeps a Machine running:
// always when it declares process groups, since a group no service lets stop
// keeps its Machines running; otherwise when there is no service, or any
// service keeps one running.
func requireStarted(services []flyService, processes json.RawMessage) bool {
	keeps := len(services) == 0 || processes != nil
	for _, s := range services {
		keeps = keeps || s.Processes != nil || s.keepsRunning()
	}
	return keeps
}

// flyListArgs and flyDeployArgs are the only flyctl invocations deploy
// makes.
func flyListArgs(kind, app string) []string {
	return []string{kind, "list", "--app", app, "--json"}
}

func flyDeployArgs(config, app, image string) []string {
	return []string{"deploy", "--config", config, "--app", app, "--image", image}
}

func (a *attempt) flyEnv() []string {
	return []string{"FLY_API_TOKEN=" + a.secrets.flyToken, "FLY_NO_UPDATE_CHECK=1"}
}

// snapshot records every Machine and volume of the app. Each flyctl call gets
// the usual deadline, cut short by budget when budget is set.
func (a *attempt) snapshot(ctx context.Context, budget time.Time) (*targets, error) {
	list := func(kind string) ([]byte, error) {
		var limit time.Duration
		if !budget.IsZero() {
			if limit = time.Until(budget); limit <= 0 {
				return nil, errors.New("verification time is used up")
			}
		}
		return a.r.run(ctx, childSpec{
			label: "flyctl " + kind + " list", tool: "flyctl",
			args: flyListArgs(kind, a.app),
			dir:  a.work, env: a.flyEnv(), budget: limit, capture: true,
		})
	}
	machinesJSON, err := list("machines")
	if err != nil {
		return nil, err
	}
	volumesJSON, err := list("volumes")
	if err != nil {
		return nil, err
	}
	return parseTargets(a.app, machinesJSON, volumesJSON)
}

// parseTargets reads `flyctl machines list --json` and `flyctl volumes list
// --json`.
func parseTargets(app string, machinesJSON, volumesJSON []byte) (*targets, error) {
	var ms []struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		ImageRef struct {
			Digest string `json:"digest"`
		} `json:"image_ref"`
	}
	if err := json.Unmarshal(machinesJSON, &ms); err != nil {
		return nil, fmt.Errorf("flyctl machines list: %w", err)
	}
	var vs []struct {
		ID                string  `json:"id"`
		AttachedMachineID *string `json:"attached_machine_id"`
	}
	if err := json.Unmarshal(volumesJSON, &vs); err != nil {
		return nil, fmt.Errorf("flyctl volumes list: %w", err)
	}
	t := &targets{App: app, Machines: []machine{}, Volumes: []volume{}}
	for _, m := range ms {
		if m.ID == "" {
			return nil, errors.New("flyctl machines list: Machine without an id")
		}
		t.Machines = append(t.Machines, machine{ID: m.ID, State: m.State, ImageDigest: strPtr(m.ImageRef.Digest)})
	}
	for _, v := range vs {
		if v.ID == "" {
			return nil, errors.New("flyctl volumes list: volume without an id")
		}
		attached := ""
		if v.AttachedMachineID != nil {
			attached = *v.AttachedMachineID
		}
		t.Volumes = append(t.Volumes, volume{ID: v.ID, AttachedMachineID: strPtr(attached)})
	}
	slices.SortFunc(t.Machines, func(x, y machine) int { return strings.Compare(x.ID, y.ID) })
	slices.SortFunc(t.Volumes, func(x, y volume) int { return strings.Compare(x.ID, y.ID) })
	return t, nil
}

// inspectApp records the app before anything changes. An app without
// Machines is refused here: an ordinary deploy would create them, which
// verification rejects as a Machine count change. Under converge, it also
// decides whether the app is provably already at this commit's outputs.
func (a *attempt) inspectApp(ctx context.Context) result {
	t, err := a.snapshot(ctx, time.Time{})
	if err != nil {
		return childResult(err, false)
	}
	a.rec.TargetsBefore = t
	if len(t.Machines) == 0 {
		return failed("app %s has no Machines; deploy only updates existing ones", a.app)
	}
	if a.checkSkip {
		skip, reason := a.canSkipApp()
		if skip {
			a.skipped = true
			a.skipAfterInspect = reason
		} else {
			a.deployReason = reason
			if a.req.dryRun {
				a.skipAfterInspect = "dry run: would deploy: " + reason
			} else {
				fmt.Fprintf(a.stderr, "deploy: deploying app %s: %s\n", a.req.name, a.redact(reason))
			}
		}
	}
	return completed()
}

// skopeo runs skopeo with a private policy and registry configuration, and
// the auth file holding the Fly token as an inherited descriptor named by
// REGISTRY_AUTH_FILE. --tmpdir keeps the unpacked archive inside the attempt:
// skopeo ignores TMPDIR for it.
func (a *attempt) skopeo(ctx context.Context, label string, mutator, capture bool, args ...string) ([]byte, error) {
	containers := filepath.Join(a.work, "containers")
	spec := childSpec{
		label: label, tool: "skopeo",
		args: append([]string{
			"--policy", filepath.Join(containers, "policy.json"),
			"--registries.d", filepath.Join(containers, "registries.d"),
			"--tmpdir", a.r.tmp,
		}, args...),
		dir:     a.work,
		files:   []*os.File{a.auth},
		mutator: mutator, capture: capture,
	}
	spec.env = []string{
		fmt.Sprintf("REGISTRY_AUTH_FILE=/proc/self/fd/%d", spec.fd(0)),
		"CONTAINERS_REGISTRIES_CONF=" + filepath.Join(containers, "registries.conf"),
	}
	return a.r.run(ctx, spec)
}

// writeContainerConfig writes skopeo's private configuration: an auth file
// for the Fly registry (user x, the token as password), a policy accepting the
// locally built image, and empty registry configuration so no system mirror
// or signature store applies. The auth file is unlinked before the token is
// written to it and lives only as this process's descriptor, so no path to
// the token outlives the attempt, however it ends.
func (a *attempt) writeContainerConfig() error {
	dir := filepath.Join(a.work, "containers")
	if err := os.MkdirAll(filepath.Join(dir, "registries.d"), 0o700); err != nil {
		return err
	}
	auth, err := json.Marshal(map[string]any{"auths": map[string]any{
		registry: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("x:" + a.secrets.flyToken))},
	}})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "auth-*")
	if err != nil {
		return err
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(auth); err != nil {
		f.Close()
		return err
	}
	a.auth = f
	files := map[string][]byte{
		"policy.json":     []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`),
		"registries.conf": nil,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// uploadApp pushes the image under the attempt's tag, preserving its digest,
// then reads the manifest back and compares its SHA-256 with the local one.
// The tag is referenced by nothing, so the running app is unchanged whatever
// happens here: every failure is a known one.
func (a *attempt) uploadApp(ctx context.Context) result {
	if err := a.writeContainerConfig(); err != nil {
		return failed("write registry configuration: %v", err)
	}
	ref := "docker://" + registry + "/" + a.app + ":" + a.rec.AttemptID
	if _, err := a.skopeo(ctx, "skopeo copy", true, false, "copy", "--preserve-digests", "oci-archive:"+a.imagePath, ref); err != nil {
		return childResult(err, false)
	}
	raw, err := a.skopeo(ctx, "skopeo inspect", false, true, "inspect", "--raw", ref)
	if err != nil {
		return failed("pushed, but reading the manifest back failed: %v", err)
	}
	if got := "sha256:" + sha256Hex(raw); got != a.digest {
		return failed("registry holds manifest %s, want %s", got, a.digest)
	}
	return completed()
}

// applyApp records this attempt as the app's last mutating attempt, runs an
// ordinary fly deploy of the verified digest, and then observes the app,
// whatever the deploy's outcome. The observation never changes that outcome.
func (a *attempt) applyApp(ctx context.Context) result {
	if err := a.writeAppPointer(); err != nil {
		return failed("write app pointer: %v", err)
	}
	image := registry + "/" + a.app + "@" + a.digest
	_, err := a.r.run(ctx, childSpec{
		label:   "flyctl deploy",
		tool:    "flyctl",
		args:    flyDeployArgs(a.configFile, a.app, image),
		dir:     a.work,
		env:     a.flyEnv(),
		mutator: true,
	})
	res := childResult(err, true)
	if t, err := a.snapshot(ctx, time.Time{}); err == nil {
		a.rec.TargetsAfter = t
	}
	return res
}

// verifyApp checks the deployed app against the snapshot taken before it. A
// changed volume set fails at once. Otherwise it polls, because flyctl offers
// no event for a Machine changing image, until the app verifies or the budget
// runs out. Then the outcome is unknown, so uncertain, if the most recent
// observation failed or a Machine is still between states; otherwise it
// failed. A snapshot starts only with minObservation left, so one the window
// cuts off is a read that hung.
func (a *attempt) verifyApp(ctx context.Context) result {
	deadline := time.Now().Add(a.verifyTimeout)
	t := a.rec.TargetsAfter
	lost := t == nil // the most recent observation attempt failed
	pending := false
	why := "the app could not be observed after the deploy"
	for {
		if t != nil {
			if reason := volumesChanged(a.rec.TargetsBefore, t); reason != "" {
				return failed("%s", reason)
			}
			var reason string
			if reason, pending = judge(a.rec.TargetsBefore, t, a.digest, a.requireStarted); reason == "" {
				return completed()
			}
			why = reason
		}
		if time.Until(deadline) < a.verifyInterval+a.minObservation() {
			res := failed("not verified within %v: %s", a.verifyTimeout, why)
			if lost || pending {
				res.outcome = outcomeUncertain
			}
			return res
		}
		wait := time.NewTimer(a.verifyInterval)
		select {
		case <-ctx.Done():
			wait.Stop()
			return result{outcome: outcomeUncertain, reason: "verify " + context.Cause(ctx).Error()}
		case <-wait.C:
		}
		next, err := a.snapshot(ctx, deadline)
		if err != nil {
			if isInterrupted(err) {
				// The deploy completed; its state is now unconfirmed.
				return result{outcome: outcomeUncertain, reason: err.Error()}
			}
			why, t, lost = err.Error(), nil, true
			continue
		}
		t, lost = next, false
		a.rec.TargetsAfter = t
	}
}

// minObservation is the window a snapshot needs left to start: five poll
// intervals, ten seconds in production, far more than flyctl takes.
func (a *attempt) minObservation() time.Duration { return 5 * a.verifyInterval }

func volumesChanged(before, after *targets) string {
	ids := func(vs []volume) []string {
		out := []string{}
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return out
	}
	if b, a := ids(before.Volumes), ids(after.Volumes); !slices.Equal(b, a) {
		return fmt.Sprintf("volumes changed from %v to %v", b, a)
	}
	return ""
}

// settled are the Machine states verification judges by image. A Machine in
// any other state is between two, and pending.
var settled = map[string]bool{"started": true, "stopped": true, "suspended": true}

// judge returns why the app does not verify yet, or "" if it does: none may
// be pending, the Machine count must be as before (a Machine being replaced
// can show briefly beside its replacement), every settled Machine must report
// the deployed digest, and when the configuration keeps a Machine running one
// must be started. pending reports a Machine still between states.
func judge(before, t *targets, digest string, requireStarted bool) (reason string, pending bool) {
	started := 0
	for _, m := range t.Machines {
		if !settled[m.State] {
			return fmt.Sprintf("Machine %s is %q, between states", m.ID, m.State), true
		}
		if m.State == "started" {
			started++
		}
	}
	if b, a := len(before.Machines), len(t.Machines); b != a {
		return fmt.Sprintf("Machine count changed from %d to %d", b, a), false
	}
	for _, m := range t.Machines {
		if m.ImageDigest == nil || *m.ImageDigest != digest {
			got := "no image digest"
			if m.ImageDigest != nil {
				got = *m.ImageDigest
			}
			return fmt.Sprintf("%s Machine %s reports %s, want %s", m.State, m.ID, got, digest), false
		}
	}
	if requireStarted && started == 0 {
		return "no Machine is started, and fly.json keeps one running", false
	}
	return "", false
}

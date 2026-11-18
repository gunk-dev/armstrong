package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestApp_DeploysTheVerifiedDigest(t *testing.T) {
	h := newHarness(t)
	h.appFixture()

	o := h.app()
	wantRun(t, o, 0, outcomeCompleted, phaseNames("completed", "completed", "completed", "completed", "completed", "completed", "completed")...)
	wantCleanedUp(t, o)
	wantIsolated(t, h, o)

	// The tool calls after the source, in order: the image (and with it its
	// smoke check) is built before anything is pushed, and the app is
	// observed before and after the deploy.
	wantCalls := []string{
		"nix build", "nix build",
		"flyctl machines", "flyctl volumes",
		"skopeo copy", "skopeo inspect",
		"flyctl deploy",
		"flyctl machines", "flyctl volumes",
	}
	if got := h.toolCalls(); !slices.Equal(got, wantCalls) {
		t.Errorf("calls %q\nwant %q", got, wantCalls)
	}

	rec := o.rec
	id := filepath.Base(filepath.Dir(o.recordPath))
	tag := "docker://registry.fly.io/" + testApp + ":" + id
	image := "registry.fly.io/" + testApp + "@" + h.digest
	for _, c := range h.calls() {
		switch {
		case c.Tool == "nix":
			if c.Info["rev"] != h.rev {
				t.Errorf("nix built rev %q, want %q", c.Info["rev"], h.rev)
			}
		case c.Tool == "skopeo" && c.Args[6] == "copy":
			if want := []string{"copy", "--preserve-digests", "oci-archive:" + filepath.Join(h.out, "image.tar"), tag}; !slices.Equal(c.Args[6:], want) {
				t.Errorf("skopeo %q, want %q", c.Args[6:], want)
			}
		case c.Tool == "flyctl" && c.Args[0] == "deploy":
			want := []string{"deploy", "--config", filepath.Join(h.out, "config", "fly.json"), "--app", testApp, "--image", image}
			if !slices.Equal(c.Args, want) {
				t.Errorf("flyctl %q, want %q", c.Args, want)
			}
		}
	}
	if m := h.liveWorld().Registry[testApp+":"+id]; "sha256:"+sha256Hex(m) != h.digest {
		t.Errorf("registry tag %s holds %s, want %s", id, "sha256:"+sha256Hex(m), h.digest)
	}

	cfg, _ := os.ReadFile(filepath.Join(h.out, "config", "fly.json"))
	lock := sha256Hex([]byte(baseFiles["flake.lock"]))
	if rec.SchemaVersion != 1 || rec.AttemptID != id || rec.Verb != "app" || rec.DryRun ||
		rec.Source != h.infra || rec.RequestedCommit != h.rev ||
		rec.ResolvedCommit == nil || *rec.ResolvedCommit != h.rev ||
		rec.LockSHA256 == nil || *rec.LockSHA256 != lock ||
		!maps.Equal(rec.ConfigSHA256, map[string]string{"fly.json": sha256Hex(cfg)}) ||
		rec.Platform != testSystem || rec.RunnerVersion != "test-version" ||
		rec.ImageDigest == nil || *rec.ImageDigest != h.digest {
		t.Errorf("record identity fields wrong: %+v", rec)
	}
	for _, ts := range []*string{&rec.StartedAt, rec.FinishedAt} {
		if ts == nil {
			t.Fatal("missing timestamp")
		}
		if parsed, err := time.Parse(time.RFC3339Nano, *ts); err != nil || parsed.Location() != time.UTC {
			t.Errorf("timestamp %q is not UTC RFC 3339: %v", *ts, err)
		}
	}

	old, deployed := oldDigest, h.digest
	m1 := "m1"
	wantBefore := &targets{App: testApp,
		Machines: []machine{{ID: "m1", State: "started", ImageDigest: &old}, {ID: "m2", State: "stopped", ImageDigest: &old}},
		Volumes:  []volume{{ID: "vol1", AttachedMachineID: &m1}, {ID: "vol2"}},
	}
	wantAfter := &targets{App: testApp,
		Machines: []machine{{ID: "m1", State: "started", ImageDigest: &deployed}, {ID: "m2", State: "stopped", ImageDigest: &deployed}},
		Volumes:  wantBefore.Volumes,
	}
	if !reflect.DeepEqual(rec.TargetsBefore, wantBefore) || !reflect.DeepEqual(rec.TargetsAfter, wantAfter) {
		before, _ := json.Marshal(rec.TargetsBefore)
		after, _ := json.Marshal(rec.TargetsAfter)
		t.Errorf("targets before %s\nafter %s", before, after)
	}

	// The record's JSON shape is the documented one, field for field.
	var raw map[string]any
	data, _ := os.ReadFile(o.recordPath)
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"attempt_id", "config_sha256", "dry_run", "finished_at", "image_digest", "lock_sha256",
		"logs", "name", "outcome", "phases", "platform", "requested_commit", "resolved_commit", "runner_version",
		"schema_version", "source", "started_at", "targets_after", "targets_before", "verb"}
	if rec.Name == nil || *rec.Name != "demo" || rec.Logs != "logs" {
		t.Errorf("name %v, logs %q; want demo, logs", rec.Name, rec.Logs)
	}
	if got := slices.Sorted(maps.Keys(raw)); !slices.Equal(got, wantKeys) {
		t.Errorf("record keys %q\nwant %q", got, wantKeys)
	}
	phases, _ := raw["phases"].([]any)
	for _, p := range phases {
		phase, _ := p.(map[string]any)
		if !slices.Equal(slices.Sorted(maps.Keys(phase)), []string{"name", "outcome", "reason"}) {
			t.Errorf("phase keys %v", phase)
		}
	}
	if len(phases) != 7 {
		t.Errorf("%d phases recorded, want 7", len(phases))
	}
}

// wantIsolated checks every child ran with the pinned PATH, private
// directories and no ambient environment, that each credential reached only
// its own tool, and that none of the credentials the fakes printed reached
// deploy's output or record.
func wantIsolated(t *testing.T, h *harness, o outcome) {
	t.Helper()
	work := filepath.Join(filepath.Dir(o.recordPath), "work") + "/"
	for _, c := range h.calls() {
		if v, _ := c.env("PATH"); v != toolDir {
			t.Errorf("%s ran with PATH %q", c.Tool, v)
		}
		for _, key := range []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
			if v, _ := c.env(key); !strings.HasPrefix(v, work) {
				t.Errorf("%s ran with %s=%q, outside the attempt's work directory", c.Tool, key, v)
			}
		}
		if !strings.HasPrefix(c.Dir+"/", work) {
			t.Errorf("%s ran in %s", c.Tool, c.Dir)
		}
		for _, kv := range c.Env {
			if strings.Contains(kv, ambientMarker) {
				t.Errorf("%s received ambient %s", c.Tool, kv)
			}
		}
		for key, owner := range map[string]string{
			"FLY_API_TOKEN":      "flyctl",
			"REGISTRY_AUTH_FILE": "skopeo",
			"PORKBUN_API_KEY":    "dns",
			"PORKBUN_SECRET_KEY": "dns",
		} {
			if _, ok := c.env(key); ok != (c.Tool == owner) {
				t.Errorf("%s: %s present=%v", c.Tool, key, ok)
			}
		}
		switch c.Tool {
		case "flyctl":
			if v, _ := c.env("FLY_API_TOKEN"); v != flyToken {
				t.Errorf("flyctl got token %q", v)
			}
		case "skopeo":
			// The auth file reaches skopeo only as an inherited descriptor
			// to a file already unlinked.
			if c.Info["authUser"] != "x" || c.Info["authPass"] != flyToken || c.Info["authMode"] != "600" ||
				!strings.HasPrefix(c.Info["authFile"], "/proc/self/fd/") || !strings.HasSuffix(c.Info["authLink"], " (deleted)") ||
				!strings.HasPrefix(c.Info["tmpdir"], work) || c.Info["policy"] != "ok" {
				t.Errorf("skopeo auth and policy: %v", c.Info)
			}
		case "dns":
			k, _ := c.env("PORKBUN_API_KEY")
			s, _ := c.env("PORKBUN_SECRET_KEY")
			if k != porkbunKey || s != porkbunSecret {
				t.Errorf("dns got keys %q, %q", k, s)
			}
		}
	}
	// The fakes print the credentials they get; the logs keep their output,
	// redacted.
	logs, _ := filepath.Glob(filepath.Join(filepath.Dir(o.recordPath), "logs", "*.log"))
	for _, path := range append([]string{o.recordPath}, logs...) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{flyToken, porkbunKey, porkbunSecret} {
			for _, form := range append(encodedForms(secret), secret) {
				if strings.Contains(o.stdout+o.stderr+string(data), form) {
					t.Errorf("credential %q, as %q, leaked into deploy's output or %s", secret, form, path)
				}
			}
		}
	}
	if len(logs) == 0 || !strings.Contains(readAll(t, logs...), "CANARY [REDACTED]") {
		t.Error("the logs do not show the fakes' redacted canaries")
	}
}

func TestApp_MachineReplacementAllowed(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	// The deploy replaces every Machine, and the first observation after it
	// still shows the old ones, so verification has to poll.
	h.sc.Deploy = deployBehaviour{Effect: "replace", Lag: 1}

	o := h.app()
	wantRun(t, o, 0, outcomeCompleted, phaseNames("completed", "completed", "completed", "completed", "completed", "completed", "completed")...)
	var ids []string
	for _, m := range o.rec.TargetsAfter.Machines {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"m1-new", "m2-new"}) {
		t.Errorf("Machines after %v, want the replacements", ids)
	}
}

func TestApp_ImageIsBuiltAndCheckedBeforeUpload(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	// The image derivation depends on its smoke check, so a failing check
	// fails the build.
	out := h.sc.Outputs["deploy-demo-image"]
	out.Fail = true
	h.sc.Outputs["deploy-demo-image"] = out

	o := h.app()
	t.Logf("deploy printed:\n%s%s", o.stdout, o.stderr)
	wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "failed", "skipped", "skipped", "skipped", "skipped", "completed")...)
	wantCleanedUp(t, o)
	for _, name := range h.callNames() {
		if strings.HasPrefix(name, "skopeo") || strings.HasPrefix(name, "flyctl") {
			t.Errorf("%s ran after the image failed to build", name)
		}
	}
	// The failure names the build's log, which holds what nix printed.
	if log := namedLog(t, o.stderr, "nix-build-deploy-demo-image"); !strings.Contains(readAll(t, log), "smoke check failed") {
		t.Errorf("%s lacks the build's error", log)
	}
	if o.rec.ImageDigest != nil {
		t.Errorf("image_digest = %v, want null", *o.rec.ImageDigest)
	}
}

func TestApp_RejectsOutputs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
	}{
		{"multi-platform image", func(h *harness) {
			writeImage(t, h.sc.Outputs["deploy-demo-image"].Path, image{arch: "amd64", twoPlatforms: true})
		}},
		{"incompatible platform", func(h *harness) {
			writeImage(t, h.sc.Outputs["deploy-demo-image"].Path, image{arch: "arm64"})
		}},
		{"archive without blob directories", func(h *harness) {
			writeImage(t, h.sc.Outputs["deploy-demo-image"].Path, image{arch: "amd64", noDirs: true})
		}},
		{"image linking out of the store", func(h *harness) {
			outside := filepath.Join(h.root, "mutable.tar")
			writeImage(t, outside, image{arch: "amd64"})
			link := filepath.Join(h.out, "image-link.tar")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			h.sc.Outputs["deploy-demo-image"] = fakeOutput{Path: link}
		}},
		{"config directory linking out of the store", func(h *harness) {
			outside := h.write("mutable-config/fly.json", `{"app":"demo-app"}`)
			link := filepath.Join(h.out, "config-link")
			if err := os.Symlink(filepath.Dir(outside), link); err != nil {
				t.Fatal(err)
			}
			h.sc.Outputs["deploy-demo-config"] = fakeOutput{Path: link}
		}},
		{"fly.json linking out of the store", func(h *harness) {
			outside := h.write("mutable.json", `{"app":"demo-app"}`)
			cfg := filepath.Join(h.out, "config", "fly.json")
			os.Remove(cfg)
			if err := os.Symlink(outside, cfg); err != nil {
				t.Fatal(err)
			}
		}},
		{"archive with two indexes", func(h *harness) {
			writeImage(t, h.sc.Outputs["deploy-demo-image"].Path, image{arch: "amd64", twoIndexes: true})
		}},
		{"config beside fly.json", func(h *harness) {
			h.writeOut("config/fly.toml", "app = 'demo-app'\n")
		}},
		{"fly.json without an app", func(h *harness) {
			h.writeOut("config/fly.json", `{"primary_region":"ord"}`)
		}},
		{"build rewrites flake.lock", func(h *harness) {
			out := h.sc.Outputs["deploy-demo-image"]
			out.TouchLock = true
			h.sc.Outputs["deploy-demo-image"] = out
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.appFixture()
			tc.setup(h)

			o := h.app()
			wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "failed", "skipped", "skipped", "skipped", "skipped", "completed")...)
			for _, name := range h.callNames() {
				if strings.HasPrefix(name, "skopeo") || strings.HasPrefix(name, "flyctl") {
					t.Errorf("%s ran after prepare failed", name)
				}
			}
		})
	}
}

func TestApp_RemoteDigestMismatchStopsBeforeDeploy(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	h.sc.CorruptPush = true

	o := h.app()
	wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "completed", "completed", "failed", "skipped", "skipped", "completed")...)
	if slices.Contains(h.callNames(), "flyctl deploy") {
		t.Error("deployed an image whose remote manifest differs from the local build")
	}
}

func TestApp_InspectRefusesAMissingApp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
	}{
		{"app does not exist", func(h *harness) { h.sc.ListFail = true }},
		{"app has no Machines", func(h *harness) { h.world.Machines = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.appFixture()
			tc.setup(h)

			o := h.app()
			wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "completed", "failed", "skipped", "skipped", "skipped", "completed")...)
			for _, name := range h.callNames() {
				if strings.HasPrefix(name, "skopeo") || name == "flyctl deploy" {
					t.Errorf("%s ran although the app cannot be deployed", name)
				}
			}
		})
	}
}

func TestApp_VerifyFailures(t *testing.T) {
	for _, effect := range []string{"drop-volume", "add-machine", "stale", "stop-all"} {
		t.Run(effect, func(t *testing.T) {
			h := newHarness(t)
			h.appFixture()
			h.sc.Deploy.Effect = effect
			// A changed topology fails at once; a Machine that never reports
			// the digest, or none started, fails when the budget runs out.
			h.bin = shortVerifyBin

			o := h.app()
			wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "completed", "completed", "completed", "completed", "failed", "completed")...)
			if o.rec.TargetsAfter == nil {
				t.Error("targets_after not recorded")
			}
		})
	}
}

// letsEveryMachineStop is a fly.json whose service stops idle Machines and
// keeps none running.
const letsEveryMachineStop = `{"app":"demo-app","http_service":{"internal_port":8080,"auto_stop_machines":"suspend","min_machines_running":0}}`

// TestApp_IdleMachines covers stopped and suspended Machines: each must report
// the deployed digest like a started one, and a started Machine is required
// only when fly.json keeps one running.
func TestApp_IdleMachines(t *testing.T) {
	allSuspended := []fakeMachine{{ID: "m1", State: "suspended", Digest: oldDigest}, {ID: "m2", State: "suspended", Digest: oldDigest}}
	for _, tc := range []struct {
		name     string
		config   string // fly.json; empty for the fixture's, which keeps a Machine running
		machines []fakeMachine
		effect   string
		want     string
	}{
		{"updated beside a started Machine", "", nil, "", outcomeCompleted},
		{"left on the old image beside a started Machine", "", nil, "skip-idle", outcomeFailed},
		{"all suspended, fly.json keeps one running", "", allSuspended, "", outcomeFailed},
		{"all suspended, fly.json lets every Machine stop", letsEveryMachineStop, allSuspended, "", outcomeCompleted},
		{"all suspended on the old image, fly.json lets every Machine stop", letsEveryMachineStop, allSuspended, "skip-idle", outcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.appFixture()
			h.world.Machines = append(h.world.Machines, fakeMachine{ID: "m3", State: "suspended", Digest: oldDigest})
			if tc.machines != nil {
				h.world.Machines = tc.machines
			}
			if tc.config != "" {
				h.writeOut("config/fly.json", tc.config)
			}
			h.sc.Deploy.Effect = tc.effect
			h.bin = shortVerifyBin

			o := h.app()
			code, verify := 0, outcomeCompleted
			if tc.want != outcomeCompleted {
				code, verify = 1, tc.want
			}
			wantRun(t, o, code, tc.want, phaseNames("completed", "completed", "completed", "completed", "completed", verify, "completed")...)
		})
	}
}

// TestApp_MachineBetweenStatesIsUncertain leaves a Machine replacing past the
// window: whether it lands on the new image is unknown.
func TestApp_MachineBetweenStatesIsUncertain(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	h.sc.Deploy.Effect = "leave-replacing"
	h.bin = shortVerifyBin

	o := h.app()
	wantRun(t, o, 1, outcomeUncertain, phaseNames("completed", "completed", "completed", "completed", "completed", "uncertain", "completed")...)
}

// TestApp_HungObservationIsUncertain sees the old image once after the
// deploy, then a read hangs until the window closes: the outcome is unknown.
func TestApp_HungObservationIsUncertain(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	h.sc.Deploy = deployBehaviour{Lag: 1000, BreakLists: true, ListsBefore: 2, HangLists: true}
	h.bin = shortVerifyBin

	o := h.app()
	wantRun(t, o, 1, outcomeUncertain, phaseNames("completed", "completed", "completed", "completed", "completed", "uncertain", "completed")...)
}

// TestApp_TransientExtraMachine shows the first snapshot after the deploy a
// Machine still being destroyed beside its replacement: the count differs
// until it settles, and verify waits for that.
func TestApp_TransientExtraMachine(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	h.sc.Deploy.Effect = "transient-extra"

	o := h.app()
	wantRun(t, o, 0, outcomeCompleted, phaseNames("completed", "completed", "completed", "completed", "completed", "completed", "completed")...)
}

// TestApp_LostObservationIsUncertain sees the old image once after the
// deploy, then every read fails: the window ends with the outcome unknown.
func TestApp_LostObservationIsUncertain(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	h.sc.Deploy = deployBehaviour{Lag: 1000, BreakLists: true, ListsBefore: 2}
	h.bin = shortVerifyBin

	o := h.app()
	wantRun(t, o, 1, outcomeUncertain, phaseNames("completed", "completed", "completed", "completed", "completed", "uncertain", "completed")...)
	if after := o.rec.TargetsAfter; after == nil || after.Machines[0].ImageDigest == nil || *after.Machines[0].ImageDigest != oldDigest {
		t.Errorf("targets_after %+v, want the one observation, on the old image", after)
	}
}

// TestFlyConfig_RequireStarted covers which fly.json keeps a Machine running,
// one case per branch of the rule.
func TestFlyConfig_RequireStarted(t *testing.T) {
	t.Parallel()
	for config, want := range map[string]bool{
		`{"app":"a"}`: true, // no service
		`{"app":"a","http_service":{"auto_stop_machines":"suspend","min_machines_running":0}}`: false,
		`{"app":"a","http_service":{"auto_stop_machines":"off"}}`:                              true,
		`{"app":"a","http_service":{"auto_stop_machines":"stop","min_machines_running":1}}`:    true,
		`{"app":"a","processes":{"web":"w"},"http_service":{"auto_stop_machines":"suspend"}}`:  true,
		`{"app":"a","http_service":{"auto_stop_machines":"suspend","processes":["web"]}}`:      true,
	} {
		dir, err := os.MkdirTemp(testStore, "config-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "fly.json"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := readFlyConfig(dir)
		if err != nil || cfg.requireStarted != want || cfg.sum != sha256Hex([]byte(config)) {
			t.Errorf("%s: requireStarted %v (%v), want %v", config, cfg.requireStarted, err, want)
		}
	}
}

func TestApp_UnobservableAfterDeployIsUncertain(t *testing.T) {
	h := newHarness(t)
	h.appFixture()
	h.sc.Deploy.BreakLists = true
	h.bin = shortVerifyBin

	o := h.app()
	wantRun(t, o, 1, outcomeUncertain, phaseNames("completed", "completed", "completed", "completed", "completed", "uncertain", "completed")...)
	if o.rec.TargetsAfter != nil {
		t.Errorf("targets_after recorded without an observation: %+v", o.rec.TargetsAfter)
	}
}

// TestApp_LostResponses covers writes that land but whose tool exits nonzero:
// the phase stays uncertain whatever the app looks like afterwards.
func TestApp_LostResponses(t *testing.T) {
	t.Run("push", func(t *testing.T) {
		h := newHarness(t)
		h.appFixture()
		h.sc.CopyExit = 1

		// The pushed tag is referenced by nothing, so the app is unchanged.
		o := h.app()
		wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "completed", "completed", "failed", "skipped", "skipped", "completed")...)
		if slices.Contains(h.callNames(), "flyctl deploy") {
			t.Error("deployed after an uncertain push")
		}
		if len(h.liveWorld().Registry) != 1 {
			t.Error("the fake push did not land")
		}
	})
	t.Run("deploy", func(t *testing.T) {
		h := newHarness(t)
		h.appFixture()
		h.sc.Deploy.Exit = 1

		o := h.app()
		wantRun(t, o, 1, outcomeUncertain, phaseNames("completed", "completed", "completed", "completed", "uncertain", "skipped", "completed")...)
		// The app was observed after the attempt, and shows the deploy
		// landed; the apply phase stays uncertain regardless.
		after := o.rec.TargetsAfter
		if after == nil || len(after.Machines) == 0 || after.Machines[0].ImageDigest == nil || *after.Machines[0].ImageDigest != h.digest {
			t.Errorf("targets_after %+v does not show the landed deploy", after)
		}
	})
}

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gunk.dev/armstrong/internal/porkbunfake"
)

// convergeFixture sets up a multi-app infra repo with DNS zones and two Fly
// apps ("alpha" and "beta"), returning the fake Porkbun API and each app's
// built image digest.
func (h *harness) convergeFixture() (*porkbunfake.API, map[string]string) {
	h.t.Helper()
	api := h.dnsFixture(twoZones, twoZoneRecords)
	digests := map[string]string{}
	h.world = world{
		Apps: map[string]*appState{},
	}
	for _, spec := range []struct {
		name, flyApp, region string
	}{
		{"beta", "beta-app", "iad"},
		{"alpha", "alpha-app", "ord"},
	} {
		cfg := h.writeOut(spec.name+"/fly.json", `{"app":"`+spec.flyApp+`","primary_region":"`+spec.region+`"}`)
		img := filepath.Join(h.out, spec.name+".tar")
		digests[spec.name] = writeImage(h.t, img, image{arch: "amd64", layer: "-" + spec.name})
		h.sc.Outputs["deploy-"+spec.name+"-config"] = fakeOutput{Path: filepath.Dir(cfg)}
		h.sc.Outputs["deploy-"+spec.name+"-image"] = fakeOutput{Path: img}
		h.world.Apps[spec.flyApp] = &appState{
			Machines: []fakeMachine{
				{ID: spec.name + "-m1", State: "started", Digest: oldDigest},
				{ID: spec.name + "-m2", State: "stopped", Digest: oldDigest},
			},
			Volumes: []fakeVolume{{ID: spec.name + "-vol1", Attached: spec.name + "-m1"}},
		}
	}
	h.sc.ExtraPackages = []string{"default", "deploy", "deploy-helper"}
	return api, digests
}

// convergeRecords parses every record.json path printed on stdout by converge
// (in order) and loads each record.
func convergeRecords(t *testing.T, o outcome) ([]string, []*record) {
	t.Helper()
	var paths []string
	var recs []*record
	for _, line := range strings.Split(o.stdout, "\n") {
		if strings.HasPrefix(line, "/") && strings.HasSuffix(line, "/record.json") {
			paths = append(paths, line)
			var rec record
			if err := readJSON(line, &rec); err != nil {
				t.Fatalf("read record %s: %v", line, err)
			}
			recs = append(recs, &rec)
		}
	}
	return paths, recs
}

// wantConvergeIsolated verifies isolation and redaction across every attempt
// created during a converge run.
func wantConvergeIsolated(t *testing.T, h *harness, o outcome, recordPaths []string) {
	t.Helper()
	runsDir := filepath.Join(h.state, "runs") + "/"
	discoverPrefix := filepath.Join(h.state, ".discover-")
	for _, c := range h.calls() {
		if v, _ := c.env("PATH"); v != toolDir {
			t.Errorf("%s ran with PATH %q", c.Tool, v)
		}
		for _, key := range []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
			v, _ := c.env(key)
			if !strings.HasPrefix(v, runsDir) && !strings.HasPrefix(v, discoverPrefix) {
				t.Errorf("%s ran with %s=%q, outside private work dirs", c.Tool, key, v)
			}
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
	}
	var allFiles []string
	for _, rp := range recordPaths {
		allFiles = append(allFiles, rp)
		logs, _ := filepath.Glob(filepath.Join(filepath.Dir(rp), "logs", "*.log"))
		allFiles = append(allFiles, logs...)
	}
	for _, path := range allFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{flyToken, porkbunKey, porkbunSecret} {
			for _, form := range append(encodedForms(secret), secret) {
				if strings.Contains(o.stdout+o.stderr+string(data), form) {
					t.Errorf("credential %q, as %q, leaked into output or %s", secret, form, path)
				}
			}
		}
	}
	// Discovery scratch directory must be cleaned up.
	leftovers, _ := filepath.Glob(filepath.Join(h.state, ".discover-*"))
	if len(leftovers) != 0 {
		t.Errorf("discovery directories left behind: %v", leftovers)
	}
}

func TestDiscoverAppNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		attrs   []string
		want    []string
		wantErr string
	}{
		{
			name:  "sorted pairs and ignored extras",
			attrs: []string{"deploy-beta-image", "default", "deploy-alpha-config", "deploy", "deploy-alpha-image", "deploy-beta-config", "deploy-helper", "deploy-image"},
			want:  []string{"alpha", "beta"},
		},
		{
			name:  "no deploy apps",
			attrs: []string{"default", "dns", "unifi"},
			want:  nil,
		},
		{
			name:    "image without config",
			attrs:   []string{"deploy-alpha-image"},
			wantErr: "without deploy-alpha-config",
		},
		{
			name:    "config without image",
			attrs:   []string{"deploy-alpha-config"},
			wantErr: "without deploy-alpha-image",
		},
		{
			name:    "empty app name",
			attrs:   []string{"deploy--image", "deploy--config"},
			wantErr: "invalid app name",
		},
		{
			name:    "uppercase app name",
			attrs:   []string{"deploy-Alpha-image", "deploy-Alpha-config"},
			wantErr: "invalid app name",
		},
		{
			name:  "hyphenated app name",
			attrs: []string{"deploy-my-web-app-image", "deploy-my-web-app-config"},
			want:  []string{"my-web-app"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := discoverAppNames(tc.attrs)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConverge_DNSAndAppsInSortedOrder(t *testing.T) {
	h := newHarness(t)
	api, digests := h.convergeFixture()

	o := h.converge()
	if o.code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
	}
	paths, recs := convergeRecords(t, o)
	if len(recs) != 3 {
		t.Fatalf("got %d records (%v), want 3\nstdout:\n%s", len(recs), paths, o.stdout)
	}
	if recs[0].Verb != verbDNS || recs[0].Outcome != outcomeCompleted {
		t.Errorf("record[0] = %s %s, want dns-sync completed", recs[0].Verb, recs[0].Outcome)
	}
	if recs[1].Verb != verbApp || recs[1].Name == nil || *recs[1].Name != "alpha" || recs[1].Outcome != outcomeCompleted {
		t.Errorf("record[1] = %s %v %s, want app alpha completed", recs[1].Verb, recs[1].Name, recs[1].Outcome)
	}
	if recs[2].Verb != verbApp || recs[2].Name == nil || *recs[2].Name != "beta" || recs[2].Outcome != outcomeCompleted {
		t.Errorf("record[2] = %s %v %s, want app beta completed", recs[2].Verb, recs[2].Name, recs[2].Outcome)
	}
	for _, p := range paths {
		wantCleanedUp(t, outcome{recordPath: p})
	}
	wantConvergeIsolated(t, h, o, paths)

	// Verify DNS synced and both apps moved to their built digests.
	if len(api.Calls()) == 0 {
		t.Error("Porkbun received no writes")
	}
	w := h.liveWorld()
	for _, check := range []struct{ flyApp, name string }{{"alpha-app", "alpha"}, {"beta-app", "beta"}} {
		for _, m := range w.Apps[check.flyApp].Machines {
			if m.Digest != digests[check.name] {
				t.Errorf("%s machine %s has digest %s, want %s", check.flyApp, m.ID, m.Digest, digests[check.name])
			}
		}
	}

	// Summary lists every target with outcome and record path in order.
	for _, want := range []string{
		"summary:",
		"  dns-sync: completed " + paths[0],
		"  app alpha: completed " + paths[1],
		"  app beta: completed " + paths[2],
	} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("summary missing %q:\n%s", want, o.stdout)
		}
	}
}

func TestConverge_SkipsConvergedAppsAndRepeatsSafely(t *testing.T) {
	h := newHarness(t)
	_, digests := h.convergeFixture()

	// 1. Initial converge deploys both apps.
	o1 := h.converge()
	if o1.code != 0 {
		t.Fatalf("first converge failed (%d):\n%s\n%s", o1.code, o1.stdout, o1.stderr)
	}
	_, recs1 := convergeRecords(t, o1)
	alphaDeployID := recs1[1].AttemptID
	betaDeployID := recs1[2].AttemptID

	// 2. Unrelated infra commit: same outputs -> both apps skipped.
	h.rev = h.commit(map[string]string{"README.md": "unrelated change\n"})
	h.clearCalls()
	o2 := h.converge()
	if o2.code != 0 {
		t.Fatalf("second converge failed (%d):\n%s\n%s", o2.code, o2.stdout, o2.stderr)
	}
	paths2, recs2 := convergeRecords(t, o2)
	if len(recs2) != 3 {
		t.Fatalf("got %d records, want 3", len(recs2))
	}
	if recs2[0].Outcome != outcomeCompleted {
		t.Errorf("dns-sync outcome = %s, want completed", recs2[0].Outcome)
	}
	for i, want := range []struct{ name, matchID string }{{"alpha", alphaDeployID}, {"beta", betaDeployID}} {
		rec := recs2[i+1]
		if rec.Outcome != outcomeSkipped {
			t.Errorf("%s outcome = %s, want skipped", want.name, rec.Outcome)
		}
		oRec := outcome{rec: rec}
		if got, wantPhases := oRec.phases(), phaseNames("completed", "completed", "completed", "skipped", "skipped", "skipped", "completed"); !slices.Equal(got, wantPhases) {
			t.Errorf("%s phases = %q, want %q", want.name, got, wantPhases)
		}
		if reason := oRec.reason("apply"); !strings.Contains(reason, want.matchID) || !strings.Contains(reason, "image digest") || !strings.Contains(reason, "fly.json") {
			t.Errorf("%s skip reason %q should state what was compared and cite %s", want.name, reason, want.matchID)
		}
	}
	for _, callName := range h.callNames() {
		if strings.HasPrefix(callName, "skopeo") || callName == "flyctl deploy" {
			t.Errorf("skipped converge still ran %s", callName)
		}
	}
	if !strings.Contains(o2.stdout, "  app alpha: skipped (live Machines match image digest and last deploy's fly.json hash (attempt "+alphaDeployID+")) "+paths2[1]) {
		t.Errorf("summary missing expected skipped alpha reason:\n%s", o2.stdout)
	}

	// 2b. Standalone `deploy app` deploys unconditionally even when converge
	// would skip (the operator's override for out-of-band Fly config drift).
	h.clearCalls()
	oForced := h.run("app", "alpha", "--infra", h.infra, "--rev", h.rev,
		"--fly-token-file", filepath.Join(h.root, "creds", "fly-token"))
	if oForced.code != 0 || oForced.rec == nil || oForced.rec.Outcome != outcomeCompleted {
		t.Fatalf("standalone app deploy failed: code=%d rec=%v\n%s", oForced.code, oForced.rec, oForced.stderr)
	}
	if !slices.Contains(h.callNames(), "flyctl deploy") {
		t.Errorf("standalone deploy app did not run flyctl deploy: %q", h.callNames())
	}

	// 3. Repeat converge at the exact same commit (e.g., host restart): prior
	// skipped records do not hide the earlier completed records, so both apps
	// still skip.
	h.clearCalls()
	o3 := h.converge()
	if o3.code != 0 {
		t.Fatalf("third converge failed (%d):\n%s\n%s", o3.code, o3.stdout, o3.stderr)
	}
	_, recs3 := convergeRecords(t, o3)
	if recs3[1].Outcome != outcomeSkipped || recs3[2].Outcome != outcomeSkipped {
		t.Errorf("repeat converge outcomes: alpha=%s beta=%s, want skipped/skipped", recs3[1].Outcome, recs3[2].Outcome)
	}

	// 4. Change only beta's fly.json: alpha skips, beta deploys.
	h.writeOut("beta/fly.json", `{"app":"beta-app","primary_region":"ord"}`)
	h.clearCalls()
	o4 := h.converge()
	if o4.code != 0 {
		t.Fatalf("fourth converge failed (%d):\n%s\n%s", o4.code, o4.stdout, o4.stderr)
	}
	_, recs4 := convergeRecords(t, o4)
	if recs4[1].Outcome != outcomeSkipped || recs4[2].Outcome != outcomeCompleted {
		t.Errorf("selective converge outcomes: alpha=%s beta=%s, want skipped/completed", recs4[1].Outcome, recs4[2].Outcome)
	}
	// Verify beta deployed with the same image digest and alpha was untouched.
	if recs4[2].ImageDigest == nil || *recs4[2].ImageDigest != digests["beta"] {
		t.Errorf("beta digest = %v, want %s", recs4[2].ImageDigest, digests["beta"])
	}
	var deployedApps []string
	for _, c := range h.calls() {
		if c.Tool == "flyctl" && len(c.Args) >= 5 && c.Args[0] == "deploy" {
			deployedApps = append(deployedApps, c.Args[4])
		}
	}
	if !slices.Equal(deployedApps, []string{"beta-app"}) {
		t.Errorf("deployed apps %q, want only beta-app", deployedApps)
	}
}

// TestConverge_SkipRuleBranches tests every "in doubt, deploy" branch of the
// pointer-based skip rule, plus the positive controls where skipping must still
// happen.
func TestConverge_SkipRuleBranches(t *testing.T) {
	writeFakeRecord := func(t *testing.T, stateDir string, rec record) {
		t.Helper()
		dir := filepath.Join(stateDir, "runs", rec.AttemptID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := rec.marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "record.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePointer := func(t *testing.T, stateDir, flyApp, id string) {
		t.Helper()
		dir := filepath.Join(stateDir, "apps")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, flyApp), []byte(id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	singleAppConverge := func(t *testing.T, config string, machines []fakeMachine) (*harness, string) {
		t.Helper()
		h := newHarness(t)
		h.dnsFixture(twoZones, twoZoneRecords)
		if config == "" {
			config = `{"app":"alpha-app","primary_region":"ord"}`
		}
		cfg := h.writeOut("alpha/fly.json", config)
		img := filepath.Join(h.out, "alpha.tar")
		digest := writeImage(t, img, image{arch: "amd64", layer: "-alpha"})
		h.sc.Outputs["deploy-alpha-config"] = fakeOutput{Path: filepath.Dir(cfg)}
		h.sc.Outputs["deploy-alpha-image"] = fakeOutput{Path: img}
		if machines == nil {
			machines = []fakeMachine{
				{ID: "m1", State: "started", Digest: oldDigest},
				{ID: "m2", State: "stopped", Digest: oldDigest},
			}
		}
		h.world = world{
			Apps: map[string]*appState{
				"alpha-app": {Machines: machines, Volumes: []fakeVolume{{ID: "vol1", Attached: "m1"}}},
			},
		}
		return h, digest
	}

	for _, tc := range []struct {
		name        string
		config      string
		machines    []fakeMachine
		mutate      func(t *testing.T, h *harness, firstRec *record, digest string)
		wantOutcome string
	}{
		{
			name:        "missing pointer deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				if err := os.Remove(filepath.Join(h.state, "apps", "alpha-app")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:        "unreadable pointer (mode 0000) deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses file mode 0000")
				}
				p := filepath.Join(h.state, "apps", "alpha-app")
				if err := os.Chmod(p, 0o000); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:        "unreadable pointer (symlink) deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, first *record, _ string) {
				target := filepath.Join(h.root, "pointer-target")
				os.WriteFile(target, []byte(first.AttemptID+"\n"), 0o600)
				p := filepath.Join(h.state, "apps", "alpha-app")
				os.Remove(p)
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:        "dangling pointer deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				writePointer(t, h.state, "alpha-app", "does-not-exist")
			},
		},
		{
			name:        "corrupt pointed record deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, first *record, _ string) {
				os.WriteFile(filepath.Join(h.state, "runs", first.AttemptID, "record.json"), []byte("{not json"), 0o600)
			},
		},
		{
			name:        "unrelated corrupt record in runs still skips",
			wantOutcome: outcomeSkipped,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				dir := filepath.Join(h.state, "runs", "corrupt")
				os.MkdirAll(dir, 0o700)
				os.WriteFile(filepath.Join(dir, "record.json"), []byte("{not json"), 0o600)
			},
		},
		{
			name:        "uncertain attempt reaching apply updates pointer and deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				// Run a real standalone deploy that exits nonzero in apply.
				h.sc.AppDeploys = map[string]deployBehaviour{"alpha-app": {Exit: 1}}
				oUnc := h.run("app", "alpha", "--infra", h.infra, "--rev", h.rev,
					"--fly-token-file", filepath.Join(h.root, "creds", "fly-token"))
				if oUnc.rec == nil || oUnc.rec.Outcome != outcomeUncertain {
					t.Fatalf("setup uncertain deploy: code=%d rec=%v", oUnc.code, oUnc.rec)
				}
				h.sc.AppDeploys = nil
			},
		},
		{
			name:        "failed attempt after apply updates pointer and deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				// Run a real standalone deploy whose verify phase fails (stale
				// Machine left on oldDigest).
				w := h.liveWorld()
				w.Apps["alpha-app"].Machines[0].Digest = oldDigest
				h.saveLiveWorld(w)
				h.sc.AppDeploys = map[string]deployBehaviour{"alpha-app": {Effect: "stale"}}
				prevBin := h.bin
				h.bin = shortVerifyBin
				oFail := h.run("app", "alpha", "--infra", h.infra, "--rev", h.rev,
					"--fly-token-file", filepath.Join(h.root, "creds", "fly-token"))
				h.bin = prevBin
				if oFail.rec == nil || oFail.rec.Outcome != outcomeFailed {
					t.Fatalf("setup verify-failed deploy: code=%d rec=%v", oFail.code, oFail.rec)
				}
				h.sc.AppDeploys = nil
				// Restore live Machine digest so only the pointer decides.
				w = h.liveWorld()
				for i := range w.Apps["alpha-app"].Machines {
					w.Apps["alpha-app"].Machines[i].Digest = *oFail.rec.ImageDigest
				}
				h.saveLiveWorld(w)
			},
		},
		{
			name:        "failed attempt before apply preserves pointer and still skips",
			wantOutcome: outcomeSkipped,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				// Fail in prepare (never reaches apply).
				imgOut := h.sc.Outputs["deploy-alpha-image"]
				imgOut.Fail = true
				h.sc.Outputs["deploy-alpha-image"] = imgOut
				oFail := h.run("app", "alpha", "--infra", h.infra, "--rev", h.rev,
					"--fly-token-file", filepath.Join(h.root, "creds", "fly-token"))
				if oFail.rec == nil || oFail.rec.Outcome != outcomeFailed {
					t.Fatalf("setup prepare-failed deploy: code=%d rec=%v", oFail.code, oFail.rec)
				}
				imgOut.Fail = false
				h.sc.Outputs["deploy-alpha-image"] = imgOut
			},
		},
		{
			name:        "pointer wins even when its started_at is earlier than an older completed deploy",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, first *record, _ string) {
				started, err := time.Parse(time.RFC3339Nano, first.StartedAt)
				if err != nil {
					t.Fatal(err)
				}
				name := "alpha"
				writeFakeRecord(t, h.state, record{
					SchemaVersion: 1, AttemptID: "clock-backwards-uncertain", Verb: verbApp, Name: &name,
					Platform:  testSystem,
					StartedAt: started.Add(-time.Hour).Format(time.RFC3339Nano),
					Outcome:   outcomeUncertain,
					Phases:    []phaseRecord{{Name: "apply", Outcome: outcomeUncertain}},
				})
				writePointer(t, h.state, "alpha-app", "clock-backwards-uncertain")
			},
		},
		{
			name:        "uncertain attempt for another app still skips",
			wantOutcome: outcomeSkipped,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				other := "other"
				writeFakeRecord(t, h.state, record{
					SchemaVersion: 1, AttemptID: "other-uncertain", Verb: verbApp, Name: &other,
					Platform: testSystem, StartedAt: timestamp(), Outcome: outcomeUncertain,
					Phases: []phaseRecord{{Name: "apply", Outcome: outcomeUncertain}},
				})
				writePointer(t, h.state, "other-app", "other-uncertain")
			},
		},
		{
			name:        "fly.json changed deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				h.writeOut("alpha/fly.json", `{"app":"alpha-app","primary_region":"iad"}`)
			},
		},
		{
			name:        "image digest changed deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				writeImage(t, filepath.Join(h.out, "alpha.tar"), image{arch: "amd64", layer: "-alpha-v2"})
			},
		},
		{
			name:        "different Fly app in fly.json deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, digest string) {
				h.writeOut("alpha/fly.json", `{"app":"alpha-app-2","primary_region":"ord"}`)
				w := h.liveWorld()
				w.Apps["alpha-app-2"] = &appState{
					Machines: []fakeMachine{{ID: "m9", State: "started", Digest: digest}},
				}
				h.saveLiveWorld(w)
			},
		},
		{
			name:        "live Machine on different image deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				w := h.liveWorld()
				w.Apps["alpha-app"].Machines[1].Digest = oldDigest
				h.saveLiveWorld(w)
			},
		},
		{
			name:        "live Machine in unsettled state deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				w := h.liveWorld()
				w.Apps["alpha-app"].Machines[0].State = "replacing"
				h.saveLiveWorld(w)
			},
		},
		{
			name:        "all live Machines stopped when fly.json keeps one running deploys",
			wantOutcome: outcomeCompleted,
			mutate: func(t *testing.T, h *harness, _ *record, _ string) {
				w := h.liveWorld()
				for i := range w.Apps["alpha-app"].Machines {
					w.Apps["alpha-app"].Machines[i].State = "stopped"
				}
				h.saveLiveWorld(w)
			},
		},
		{
			name:        "all live Machines suspended when fly.json lets every Machine stop skips",
			config:      `{"app":"alpha-app","http_service":{"internal_port":8080,"auto_stop_machines":"suspend","min_machines_running":0}}`,
			machines:    []fakeMachine{{ID: "m1", State: "suspended", Digest: oldDigest}, {ID: "m2", State: "suspended", Digest: oldDigest}},
			wantOutcome: outcomeSkipped,
			mutate:      func(*testing.T, *harness, *record, string) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, digest := singleAppConverge(t, tc.config, tc.machines)
			o1 := h.converge()
			if o1.code != 0 {
				t.Fatalf("initial converge failed (%d):\n%s\n%s", o1.code, o1.stdout, o1.stderr)
			}
			_, recs1 := convergeRecords(t, o1)
			tc.mutate(t, h, recs1[1], digest)
			h.clearCalls()

			// For the two cases where live Machines are replacing/stopped, fake
			// landDeploy preserves existing Machine states, so a real deploy
			// would end failed/uncertain during verify; we still want to prove
			// converge decided to deploy (ran flyctl deploy) rather than skip.
			if tc.name == "live Machine in unsettled state deploys" || tc.name == "all live Machines stopped when fly.json keeps one running deploys" {
				h.bin = shortVerifyBin
				o2 := h.converge()
				_, recs2 := convergeRecords(t, o2)
				if len(recs2) != 2 || recs2[1].Outcome == outcomeSkipped {
					t.Fatalf("expected deploy attempt, got outcome %v\nstdout:\n%s", recs2, o2.stdout)
				}
				if !slices.Contains(h.callNames(), "flyctl deploy") {
					t.Errorf("did not run flyctl deploy: %q", h.callNames())
				}
				return
			}

			o2 := h.converge()
			if o2.code != 0 {
				t.Fatalf("second converge failed (%d):\n%s\n%s", o2.code, o2.stdout, o2.stderr)
			}
			_, recs2 := convergeRecords(t, o2)
			if len(recs2) != 2 {
				t.Fatalf("got %d records, want 2\nstdout:\n%s\nstderr:\n%s", len(recs2), o2.stdout, o2.stderr)
			}
			if recs2[1].Outcome != tc.wantOutcome {
				t.Errorf("outcome = %s, want %s\nstdout:\n%s\nstderr:\n%s", recs2[1].Outcome, tc.wantOutcome, o2.stdout, o2.stderr)
			}
			ranDeploy := slices.Contains(h.callNames(), "flyctl deploy")
			if wantDeploy := tc.wantOutcome == outcomeCompleted; ranDeploy != wantDeploy {
				t.Errorf("ran flyctl deploy = %v, want %v (calls: %q)", ranDeploy, wantDeploy, h.callNames())
			}
		})
	}
}

func TestConverge_DiscoveryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(h *harness)
		wantError []string
	}{
		{
			name:  "image without config",
			setup: func(h *harness) { delete(h.sc.Outputs, "deploy-alpha-config") },
		},
		{
			name:  "config without image",
			setup: func(h *harness) { delete(h.sc.Outputs, "deploy-alpha-image") },
		},
		{
			name:  "invalid app name",
			setup: func(h *harness) { h.sc.ExtraPackages = append(h.sc.ExtraPackages, "deploy-Bad-image", "deploy-Bad-config") },
		},
		{
			name:  "nix eval fails",
			setup: func(h *harness) { h.sc.EvalFail = true },
		},
		{
			name:  "nix eval rewrites flake.lock",
			setup: func(h *harness) { h.sc.EvalTouchLock = true },
		},
		{
			name: "two outputs target same Fly app",
			setup: func(h *harness) {
				h.writeOut("alpha/fly.json", `{"app":"shared-app","primary_region":"ord"}`)
				h.writeOut("beta/fly.json", `{"app":"shared-app","primary_region":"iad"}`)
			},
			wantError: []string{"deploy-alpha-config", "deploy-beta-config", "shared-app"},
		},
		{
			name: "config build rewrites flake.lock",
			setup: func(h *harness) {
				out := h.sc.Outputs["deploy-alpha-config"]
				out.TouchLock = true
				h.sc.Outputs["deploy-alpha-config"] = out
			},
		},
		{
			name: "config output invalid fly.json",
			setup: func(h *harness) {
				h.writeOut("alpha/fly.json", `{"app":"Not_Valid"}`)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			api, _ := h.convergeFixture()
			tc.setup(h)

			o := h.converge()
			if o.code != exitFailure {
				t.Errorf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
			}
			for _, sub := range tc.wantError {
				if !strings.Contains(o.stderr, sub) {
					t.Errorf("stderr missing %q:\n%s", sub, o.stderr)
				}
			}
			if api.Requests() != 0 {
				t.Errorf("Porkbun received %d requests after discovery failed", api.Requests())
			}
			if runs, _ := filepath.Glob(filepath.Join(h.state, "runs", "*")); len(runs) != 0 {
				t.Errorf("discovery refusal created runs: %v", runs)
			}
			if leftovers, _ := filepath.Glob(filepath.Join(h.state, ".discover-*")); len(leftovers) != 0 {
				t.Errorf("discovery directories left behind: %v", leftovers)
			}
		})
	}
}

func TestConverge_CommitFile(t *testing.T) {
	setupMainRepo := func(t *testing.T) (*harness, *porkbunfake.API, string, string, string) {
		t.Helper()
		h := newHarness(t)
		api, _ := h.convergeFixture()
		h.gitIn(h.infra, "branch", "-M", "main")
		rev1 := h.rev
		h.gitIn(h.infra, "checkout", "--quiet", "-b", "side")
		sideRev := h.commit(map[string]string{"marker": "side"})
		h.gitIn(h.infra, "checkout", "--quiet", "main")
		rev2 := h.commit(map[string]string{"marker": "main-two"})
		return h, api, rev1, rev2, sideRev
	}

	convergeWithFile := func(h *harness, content string) outcome {
		h.t.Helper()
		p := h.write("trigger/deploy.env", content)
		return h.run("converge", "--infra", "file://"+h.infra, "--commit-file", p,
			"--fly-token-file", filepath.Join(h.root, "creds", "fly-token"),
			"--porkbun-api-key-file", filepath.Join(h.root, "creds", "porkbun-key"),
			"--porkbun-secret-key-file", filepath.Join(h.root, "creds", "porkbun-secret"))
	}

	t.Run("head of main succeeds and ignores extra names", func(t *testing.T) {
		h, _, _, rev2, _ := setupMainRepo(t)
		o := convergeWithFile(h, "BASH_ENV=/nonexistent/pwn.sh\nVALLEY_REPOSITORY=infra\nEXTRA=ignored\nVALLEY_COMMIT="+rev2+"\n")
		if o.code != 0 {
			t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
		}
		_, recs := convergeRecords(t, o)
		if len(recs) != 3 || recs[0].RequestedCommit != rev2 || *recs[0].ResolvedCommit != rev2 {
			t.Errorf("records = %+v, want 3 records at %s", recs, rev2)
		}
	})

	t.Run("strict ancestor of main succeeds", func(t *testing.T) {
		h, _, rev1, _, _ := setupMainRepo(t)
		o := convergeWithFile(h, "VALLEY_COMMIT="+rev1+"\n")
		if o.code != 0 {
			t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
		}
		_, recs := convergeRecords(t, o)
		if len(recs) != 3 || recs[0].RequestedCommit != rev1 || *recs[0].ResolvedCommit != rev1 {
			t.Errorf("records = %+v, want 3 records at %s", recs, rev1)
		}
	})

	t.Run("side branch commit refused before any attempt starts", func(t *testing.T) {
		h, api, _, _, sideRev := setupMainRepo(t)
		o := convergeWithFile(h, "VALLEY_COMMIT="+sideRev+"\n")
		if o.code != exitFailure {
			t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
		}
		if !strings.Contains(o.stderr, "not an ancestor of refs/heads/main") {
			t.Errorf("stderr did not explain ancestry refusal:\n%s", o.stderr)
		}
		if api.Requests() != 0 {
			t.Errorf("Porkbun received %d requests after ancestry refusal", api.Requests())
		}
		if runs, _ := filepath.Glob(filepath.Join(h.state, "runs", "*")); len(runs) != 0 {
			t.Errorf("ancestry refusal created runs: %v", runs)
		}

		// Negative control: --rev on the same side-branch commit succeeds.
		h.rev = sideRev
		oRev := h.converge()
		if oRev.code != 0 {
			t.Fatalf("negative control (--rev on side branch) failed (%d):\n%s\n%s", oRev.code, oRev.stdout, oRev.stderr)
		}
	})

	t.Run("unknown commit refused before any attempt starts", func(t *testing.T) {
		h, api, _, _, _ := setupMainRepo(t)
		o := convergeWithFile(h, "VALLEY_COMMIT="+strings.Repeat("1", 40)+"\n")
		if o.code != exitFailure {
			t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
		}
		if api.Requests() != 0 {
			t.Errorf("Porkbun received %d requests", api.Requests())
		}
		if runs, _ := filepath.Glob(filepath.Join(h.state, "runs", "*")); len(runs) != 0 {
			t.Errorf("unknown commit created runs: %v", runs)
		}
	})

	t.Run("repo without refs/heads/main refused before any attempt starts", func(t *testing.T) {
		h, api, _, rev2, _ := setupMainRepo(t)
		h.gitIn(h.infra, "branch", "-m", "main", "other")
		o := convergeWithFile(h, "VALLEY_COMMIT="+rev2+"\n")
		if o.code != exitFailure {
			t.Fatalf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
		}
		if api.Requests() != 0 {
			t.Errorf("Porkbun received %d requests", api.Requests())
		}
		if runs, _ := filepath.Glob(filepath.Join(h.state, "runs", "*")); len(runs) != 0 {
			t.Errorf("missing main ref created runs: %v", runs)
		}
	})
}

func TestConverge_DNSOnlyFlake(t *testing.T) {
	h := newHarness(t)
	api := h.dnsFixture(twoZones, twoZoneRecords)
	h.sc.ExtraPackages = []string{"default"}

	o := h.converge()
	if o.code != 0 {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
	}
	paths, recs := convergeRecords(t, o)
	if len(recs) != 1 || recs[0].Verb != verbDNS || recs[0].Outcome != outcomeCompleted {
		t.Fatalf("records = %+v, want one completed dns-sync", recs)
	}
	if len(api.Calls()) == 0 {
		t.Error("Porkbun received no writes")
	}
	if !strings.Contains(o.stdout, "  dns-sync: completed "+paths[0]) {
		t.Errorf("summary missing dns-sync:\n%s", o.stdout)
	}
}

func TestConverge_StopsOnFirstFailureOrUncertain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(h *harness, api *porkbunfake.API)
		wantCount int
		wantLines []string
	}{
		{
			name: "dns-sync fails",
			setup: func(h *harness, _ *porkbunfake.API) {
				h.rev = h.commit(map[string]string{
					"dns/a.dev/zone.cue": zoneCUE("a.dev", "int & >=60", `{type: "A", name: "", content: "1.1.1.1", ttl: 5}`),
				})
			},
			wantCount: 1,
			wantLines: []string{"  dns-sync: failed ", "  app alpha: not attempted", "  app beta: not attempted"},
		},
		{
			name: "dns-sync uncertain",
			setup: func(_ *harness, api *porkbunfake.API) {
				api.FailOn("create b.dev")
			},
			wantCount: 1,
			wantLines: []string{"  dns-sync: uncertain ", "  app alpha: not attempted", "  app beta: not attempted"},
		},
		{
			name: "first app fails",
			setup: func(h *harness, _ *porkbunfake.API) {
				out := h.sc.Outputs["deploy-alpha-image"]
				out.Fail = true
				h.sc.Outputs["deploy-alpha-image"] = out
			},
			wantCount: 2,
			wantLines: []string{"  dns-sync: completed ", "  app alpha: failed ", "  app beta: not attempted"},
		},
		{
			name: "first app uncertain",
			setup: func(h *harness, _ *porkbunfake.API) {
				h.sc.AppDeploys = map[string]deployBehaviour{"alpha-app": {Exit: 1}}
			},
			wantCount: 2,
			wantLines: []string{"  dns-sync: completed ", "  app alpha: uncertain ", "  app beta: not attempted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			api, _ := h.convergeFixture()
			tc.setup(h, api)

			o := h.converge()
			if o.code != exitFailure {
				t.Errorf("exit %d, want 1\nstdout:\n%s\nstderr:\n%s", o.code, o.stdout, o.stderr)
			}
			_, recs := convergeRecords(t, o)
			if len(recs) != tc.wantCount {
				t.Errorf("got %d records, want %d\nstdout:\n%s", len(recs), tc.wantCount, o.stdout)
			}
			for _, want := range tc.wantLines {
				if !strings.Contains(o.stdout, want) {
					t.Errorf("summary missing %q:\n%s", want, o.stdout)
				}
			}
		})
	}
}

func TestConverge_SignalInterruptsAndStopsSequence(t *testing.T) {
	h := newHarness(t)
	h.convergeFixture()
	for _, fifo := range []string{"ready", "release"} {
		if err := syscall.Mkfifo(filepath.Join(h.root, fifo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.sc.AppDeploys = map[string]deployBehaviour{"alpha-app": {Block: true}}
	cmd := h.command(h.convergeArgs()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ready := make(chan int, 1)
	go func() {
		f, err := os.Open(filepath.Join(h.root, "ready"))
		if err != nil {
			return
		}
		data, _ := io.ReadAll(f)
		f.Close()
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		ready <- pid
	}()
	var pid int
	select {
	case pid = <-ready:
	case err := <-done:
		t.Fatalf("converge exited before alpha deploy started: %v\n%s", err, stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	o := h.finish(<-done, &stdout, &stderr)
	if o.code != exitSIGTERM {
		t.Errorf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", o.code, exitSIGTERM, o.stdout, o.stderr)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("fake flyctl deploy (pid %d) outlived converge: %v", pid, err)
	}
	_, recs := convergeRecords(t, o)
	if len(recs) != 2 || recs[0].Outcome != outcomeCompleted || recs[1].Outcome != outcomeUncertain {
		t.Errorf("records = %+v, want dns-sync completed + alpha uncertain", recs)
	}
	if !strings.Contains(o.stdout, "  app beta: not attempted") {
		t.Errorf("summary missing beta not attempted:\n%s", o.stdout)
	}
}

func TestConverge_LockContentionExits75(t *testing.T) {
	h := newHarness(t)
	h.convergeFixture()
	if err := os.MkdirAll(h.state, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := acquireLock(filepath.Join(h.state, "deploy.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	o := h.converge()
	if o.code != exitContention || len(h.calls()) != 0 {
		t.Errorf("exit %d, %d calls; want 75, 0\n%s", o.code, len(h.calls()), o.stderr)
	}
}

func TestConverge_DryRun(t *testing.T) {
	h := newHarness(t)
	api, _ := h.convergeFixture()

	// Deploy alpha once via standalone `deploy app`, leaving beta undeployed.
	oApp := h.run("app", "alpha", "--infra", h.infra, "--rev", h.rev,
		"--fly-token-file", filepath.Join(h.root, "creds", "fly-token"))
	if oApp.code != 0 {
		t.Fatalf("initial alpha deploy failed (%d):\n%s", oApp.code, oApp.stderr)
	}
	h.clearCalls()

	o := h.converge("--dry-run")
	if o.code != 0 {
		t.Fatalf("dry-run converge failed (%d):\n%s\n%s", o.code, o.stdout, o.stderr)
	}
	paths, recs := convergeRecords(t, o)
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3\nstdout:\n%s", len(recs), o.stdout)
	}
	for i, r := range recs {
		if !r.DryRun {
			t.Errorf("record[%d].dry_run = false, want true", i)
		}
	}
	if recs[0].Outcome != outcomeCompleted || recs[1].Outcome != outcomeSkipped || recs[2].Outcome != outcomeCompleted {
		t.Errorf("dry-run outcomes = %s/%s/%s, want completed/skipped/completed", recs[0].Outcome, recs[1].Outcome, recs[2].Outcome)
	}
	if len(api.Calls()) != 0 {
		t.Errorf("dry-run wrote to Porkbun: %q", api.Calls())
	}
	for _, callName := range h.callNames() {
		if strings.HasPrefix(callName, "skopeo") || callName == "flyctl deploy" {
			t.Errorf("dry-run ran mutator %s", callName)
		}
	}
	wantConvergeIsolated(t, h, o, paths)
	for _, want := range []string{
		"== a.dev ==",
		"  dns-sync: completed (dry run) " + paths[0],
		"  app alpha: would skip (live Machines match image digest and last deploy's fly.json hash (",
		"  app beta: would deploy (no previous mutating attempt recorded for this app) " + paths[2],
	} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, o.stdout)
		}
	}

	// A real converge afterwards must still skip alpha and deploy beta (the
	// dry-run record for beta must not satisfy the skip rule).
	h.clearCalls()
	oReal := h.converge()
	if oReal.code != 0 {
		t.Fatalf("real converge after dry-run failed (%d):\n%s\n%s", oReal.code, oReal.stdout, oReal.stderr)
	}
	_, recsReal := convergeRecords(t, oReal)
	if recsReal[1].Outcome != outcomeSkipped || recsReal[2].Outcome != outcomeCompleted {
		t.Errorf("real converge outcomes = %s/%s, want skipped/completed", recsReal[1].Outcome, recsReal[2].Outcome)
	}
}

// TestCanSkipApp_Unit covers pointer and record edge cases directly: missing,
// unreadable, symlink, malformed and dangling pointers, mismatched attempt_id,
// dry-run record, platform mismatch, schema version, and missing app name.
func TestCanSkipApp_Unit(t *testing.T) {
	t.Parallel()
	const (
		cfgSum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		dig    = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	baseAttempt := func(stateDir string) *attempt {
		d := dig
		return &attempt{
			req:            request{verb: verbApp, name: "alpha", stateDir: stateDir},
			system:         "x86_64-linux",
			app:            "alpha-app",
			digest:         dig,
			requireStarted: true,
			rec: &record{
				AttemptID:    "current",
				ConfigSHA256: map[string]string{"fly.json": cfgSum},
				TargetsBefore: &targets{
					App:      "alpha-app",
					Machines: []machine{{ID: "m1", State: "started", ImageDigest: &d}},
				},
			},
		}
	}
	goodRec := func() record {
		name := "alpha"
		d := dig
		return record{
			SchemaVersion: 1,
			AttemptID:     "prev",
			Verb:          verbApp,
			Name:          &name,
			Platform:      "x86_64-linux",
			ConfigSHA256:  map[string]string{"fly.json": cfgSum},
			ImageDigest:   &d,
			TargetsAfter:  &targets{App: "alpha-app"},
			StartedAt:     "2026-01-01T00:00:00Z",
			Outcome:       outcomeCompleted,
			Phases:        []phaseRecord{{Name: "apply", Outcome: outcomeCompleted}},
		}
	}
	writePtr := func(t *testing.T, stateDir, content string) {
		t.Helper()
		dir := filepath.Join(stateDir, "apps")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "alpha-app"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRec := func(t *testing.T, stateDir string, rec record) {
		t.Helper()
		dir := filepath.Join(stateDir, "runs", rec.AttemptID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := rec.marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "record.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePointedRec := func(t *testing.T, stateDir string, rec record) {
		t.Helper()
		writeRec(t, stateDir, rec)
		writePtr(t, stateDir, rec.AttemptID+"\n")
	}

	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, stateDir string)
		wantSkip bool
	}{
		{
			name:     "exact match skips",
			wantSkip: true,
			setup:    func(t *testing.T, dir string) { writePointedRec(t, dir, goodRec()) },
		},
		{
			name:     "missing pointer deploys",
			wantSkip: false,
			setup:    func(t *testing.T, dir string) { writeRec(t, dir, goodRec()) },
		},
		{
			name:     "unreadable pointer mode deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses file mode 0000")
				}
				writePointedRec(t, dir, goodRec())
				p := filepath.Join(dir, "apps", "alpha-app")
				if err := os.Chmod(p, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(p, 0o600) })
			},
		},
		{
			name:     "directory pointer deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				writeRec(t, dir, goodRec())
				if err := os.MkdirAll(filepath.Join(dir, "apps", "alpha-app"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "symlink pointer deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				writeRec(t, dir, goodRec())
				os.MkdirAll(filepath.Join(dir, "apps"), 0o700)
				target := filepath.Join(dir, "target")
				os.WriteFile(target, []byte("prev\n"), 0o600)
				if err := os.Symlink(target, filepath.Join(dir, "apps", "alpha-app")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "malformed pointer path traversal deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				writeRec(t, dir, goodRec())
				writePtr(t, dir, "../prev\n")
			},
		},
		{
			name:     "empty pointer deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				writeRec(t, dir, goodRec())
				writePtr(t, dir, "")
			},
		},
		{
			name:     "dangling pointer deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				writePtr(t, dir, "missing-attempt\n")
			},
		},
		{
			name:     "pointer naming current attempt deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				r := goodRec()
				r.AttemptID = "current"
				writePointedRec(t, dir, r)
			},
		},
		{
			name:     "mismatched attempt_id in record deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				r := goodRec()
				r.AttemptID = "other"
				recDir := filepath.Join(dir, "runs", "prev")
				os.MkdirAll(recDir, 0o700)
				data, _ := r.marshal()
				os.WriteFile(filepath.Join(recDir, "record.json"), data, 0o600)
				writePtr(t, dir, "prev\n")
			},
		},
		{
			name:     "dry-run record deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				r := goodRec()
				r.DryRun = true
				writePointedRec(t, dir, r)
			},
		},
		{
			name:     "platform mismatch deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				r := goodRec()
				r.Platform = "aarch64-linux"
				writePointedRec(t, dir, r)
			},
		},
		{
			name:     "unsupported schema version deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				r := goodRec()
				r.SchemaVersion = 2
				writePointedRec(t, dir, r)
			},
		},
		{
			name:     "missing app name deploys",
			wantSkip: false,
			setup: func(t *testing.T, dir string) {
				r := goodRec()
				r.Name = nil
				writePointedRec(t, dir, r)
			},
		},
		{
			name:     "unrelated stray file in runs still skips",
			wantSkip: true,
			setup: func(t *testing.T, dir string) {
				writePointedRec(t, dir, goodRec())
				if err := os.WriteFile(filepath.Join(dir, "runs", "stray"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, "runs", "current"), 0o700)
			tc.setup(t, dir)
			got, reason := baseAttempt(dir).canSkipApp()
			if got != tc.wantSkip {
				t.Errorf("canSkipApp() = (%v, %q), want %v", got, reason, tc.wantSkip)
			}
			if got && (!strings.Contains(reason, "image digest") || !strings.Contains(reason, "fly.json") || !strings.Contains(reason, "prev")) {
				t.Errorf("skip reason %q should state what was compared and cite attempt id", reason)
			}
		})
	}
}

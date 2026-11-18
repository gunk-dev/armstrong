package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gunk.dev/armstrong/internal/porkbunfake"
)

const cueModule = "module: \"example.com/infra\"\nlanguage: version: \"v0.9.2\"\n"

// zoneCUE is one zone package; ttl constrains every record's ttl.
func zoneCUE(domain, ttl, records string) string {
	return fmt.Sprintf("package dns\n\n#Record: {type: string, name: string, content: string, ttl: %s}\n\ndomain: %q\nrecords: [...#Record] & [%s]\n", ttl, domain, records)
}

// dnsFixture commits files to the infra repository and starts a fake Porkbun
// API holding records.
func (h *harness) dnsFixture(files map[string]string, records map[string][]porkbunfake.Record) *porkbunfake.API {
	h.t.Helper()
	requireCue(h.t)
	api := porkbunfake.New(porkbunKey, porkbunSecret, records)
	srv := httptest.NewServer(api)
	h.t.Cleanup(srv.Close)
	h.sc.PorkbunBase = srv.URL + "/api/json/v3"
	h.rev = h.commit(merge(baseFiles, map[string]string{"cue.mod/module.cue": cueModule}, files))
	return api
}

// twoZones are per-zone directories whose dotted names cue would read as
// files without a trailing slash, committed out of sorted order.
var twoZones = map[string]string{
	"dns/b.dev/zone.cue": zoneCUE("b.dev", "int", `{type: "A", name: "www", content: "2.2.2.2", ttl: 600}`),
	"dns/a.dev/zone.cue": zoneCUE("a.dev", "int", `{type: "A", name: "", content: "1.1.1.1", ttl: 600}`),
}

var twoZoneRecords = map[string][]porkbunfake.Record{
	"b.dev": {
		{ID: "7", Name: "old.b.dev", Type: "A", Content: "9.9.9.9", TTL: "600"},
		{ID: "8", Name: "b.dev", Type: "NS", Content: "ns1.porkbun.com", TTL: "86400"},
	},
}

func TestDNS_SyncsZoneDirectoriesInOrder(t *testing.T) {
	h := newHarness(t)
	// A symlink that stays inside the commit, even a dangling one, is fine.
	for name, target := range map[string]string{"flake-link": "flake.nix", "dangling": "missing"} {
		if err := os.Symlink(target, filepath.Join(h.infra, name)); err != nil {
			t.Fatal(err)
		}
	}
	api := h.dnsFixture(twoZones, twoZoneRecords)

	o := h.dnsSync()
	wantRun(t, o, 0, outcomeCompleted, phaseNames("completed", "completed", "skipped", "skipped", "completed", "skipped", "completed")...)
	wantCleanedUp(t, o)
	wantIsolated(t, h, o)

	// Zones in sorted order; with --prune the stale record goes, NS stays.
	wantAPI := []string{"create a.dev A @ 1.1.1.1", "create b.dev A www 2.2.2.2", "delete b.dev id=7"}
	if got := api.Calls(); !slices.Equal(got, wantAPI) {
		t.Errorf("Porkbun calls %q, want %q", got, wantAPI)
	}

	// Every zone is vetted before any is exported, and dns sync runs once.
	var cue []string
	for _, c := range h.calls() {
		switch c.Tool {
		case "cue":
			cue = append(cue, strings.Join(c.Args, " "))
		case "dns":
			if want := []string{"sync", "--prune"}; !slices.Equal(c.Args, want) {
				t.Errorf("dns %q, want %q", c.Args, want)
			}
		}
	}
	wantCue := []string{"vet ./dns/a.dev/", "vet ./dns/b.dev/", "export ./dns/a.dev/ --out json", "export ./dns/b.dev/ --out json"}
	if !slices.Equal(cue, wantCue) {
		t.Errorf("cue calls %q, want %q", cue, wantCue)
	}
	if n := slices.Index(h.callNames(), "dns sync"); n < 0 || slices.Contains(h.callNames()[n+1:], "dns sync") {
		t.Errorf("dns sync ran other than once: %q", h.callNames())
	}

	rec := o.rec
	if rec.Verb != "dns-sync" || rec.DryRun || len(rec.ConfigSHA256) != 1 || rec.ConfigSHA256["zones.json"] == "" ||
		rec.TargetsBefore != nil || rec.TargetsAfter != nil || rec.ImageDigest != nil {
		t.Errorf("record fields wrong: %+v", rec)
	}
}

func TestDNS_DryRunPrintsTheRedactedPlan(t *testing.T) {
	h := newHarness(t)
	api := h.dnsFixture(twoZones, twoZoneRecords)

	o := h.dnsSync("--dry-run")
	wantRun(t, o, 0, outcomeCompleted, phaseNames("completed", "completed", "completed", "skipped", "skipped", "skipped", "completed")...)
	wantIsolated(t, h, o)
	if len(api.Calls()) != 0 {
		t.Errorf("dry run wrote to Porkbun: %q", api.Calls())
	}
	if !o.rec.DryRun {
		t.Error("dry_run = false")
	}
	// The plan is printed after the record path; the keys the launcher
	// printed into it are redacted.
	for _, want := range []string{"== a.dev ==", "== b.dev ==", "DELETE A old", "[REDACTED]"} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("plan lacks %q:\n%s", want, o.stdout)
		}
	}
}

func TestDNS_FlatLayout(t *testing.T) {
	h := newHarness(t)
	api := h.dnsFixture(map[string]string{
		"dns/zone.cue":    "package dns\n\ndomain: \"gunk.dev\"\n",
		"dns/records.cue": "package dns\n\nrecords: [{type: \"A\", name: \"www\", content: \"1.2.3.4\", ttl: 600}]\n",
	}, nil)

	o := h.dnsSync()
	wantRun(t, o, 0, outcomeCompleted, phaseNames("completed", "completed", "skipped", "skipped", "completed", "skipped", "completed")...)
	if got, want := api.Calls(), []string{"create gunk.dev A www 1.2.3.4"}; !slices.Equal(got, want) {
		t.Errorf("Porkbun calls %q, want %q", got, want)
	}
}

// TestDNS_ZoneWithoutRecordsList refuses a zone with no records list, which
// dns sync --prune would read as an empty zone, and accepts an explicit one.
func TestDNS_ZoneWithoutRecordsList(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		h := newHarness(t)
		api := h.dnsFixture(map[string]string{
			"dns/a.dev/zone.cue": zoneCUE("a.dev", "int", `{type: "A", name: "", content: "1.1.1.1", ttl: 600}`),
			"dns/b.dev/zone.cue": "package dns\n\ndomain: \"b.dev\"\n",
		}, twoZoneRecords)

		o := h.dnsSync()
		wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "failed", "skipped", "skipped", "skipped", "skipped", "completed")...)
		if slices.Contains(h.callNames(), "dns sync") || api.Requests() != 0 {
			t.Errorf("dns sync ran or Porkbun was contacted: %q, %d requests", h.callNames(), api.Requests())
		}
	})
	t.Run("empty", func(t *testing.T) {
		h := newHarness(t)
		api := h.dnsFixture(map[string]string{
			"dns/b.dev/zone.cue": "package dns\n\ndomain: \"b.dev\"\nrecords: []\n",
		}, twoZoneRecords)

		o := h.dnsSync()
		wantRun(t, o, 0, outcomeCompleted, phaseNames("completed", "completed", "skipped", "skipped", "completed", "skipped", "completed")...)
		if got, want := api.Calls(), []string{"delete b.dev id=7"}; !slices.Equal(got, want) {
			t.Errorf("Porkbun calls %q, want %q", got, want)
		}
	})
}

func TestDNS_VetsEveryZoneBeforeExport(t *testing.T) {
	h := newHarness(t)
	api := h.dnsFixture(map[string]string{
		"dns/a.dev/zone.cue": zoneCUE("a.dev", "int & >=60", `{type: "A", name: "", content: "1.1.1.1", ttl: 600}`),
		"dns/b.dev/zone.cue": zoneCUE("b.dev", "int & >=60", `{type: "A", name: "", content: "2.2.2.2", ttl: 5}`),
	}, nil)

	o := h.dnsSync()
	wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "failed", "skipped", "skipped", "skipped", "skipped", "completed")...)
	if got, want := h.toolCalls(), []string{"cue vet", "cue vet"}; !slices.Equal(got, want) {
		t.Errorf("calls after source %q, want %q", got, want)
	}
	if api.Requests() != 0 {
		t.Errorf("Porkbun received %d requests", api.Requests())
	}
}

// TestDNS_RefusesSymlinks covers committed symlinks that would let cue read
// zones or imported packages from outside the commit.
func TestDNS_RefusesSymlinks(t *testing.T) {
	t.Run("through a link to the root", func(t *testing.T) {
		h := newHarness(t)
		h.write("outside/zone.cue", zoneCUE("a.dev", "int", `{type: "A", name: "", content: "6.6.6.6", ttl: 600}`))
		// x/y/up is the checkout itself, so x/y/zones looks inside but
		// resolves to the checkout's sibling, outside.
		if err := os.MkdirAll(filepath.Join(h.infra, "x", "y"), 0o755); err != nil {
			t.Fatal(err)
		}
		for link, target := range map[string]string{"x/y/up": "../..", "x/y/zones": "up/../outside", "dns": "x/y/zones"} {
			if err := os.Symlink(target, filepath.Join(h.infra, link)); err != nil {
				t.Fatal(err)
			}
		}
		api := h.dnsFixture(nil, nil)

		o := h.dnsSync()
		wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "failed", "skipped", "skipped", "skipped", "skipped", "completed")...)
		if calls := h.toolCalls(); len(calls) != 0 || api.Requests() != 0 {
			t.Errorf("ran %q and sent %d requests despite the symlink", calls, api.Requests())
		}
	})
	t.Run("zone directory linked inside the commit", func(t *testing.T) {
		h := newHarness(t)
		if err := os.MkdirAll(filepath.Join(h.infra, "dns"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../zones/b.dev", filepath.Join(h.infra, "dns", "b.dev")); err != nil {
			t.Fatal(err)
		}
		api := h.dnsFixture(map[string]string{
			"dns/a.dev/zone.cue":   zoneCUE("a.dev", "int", `{type: "A", name: "", content: "1.1.1.1", ttl: 600}`),
			"zones/b.dev/zone.cue": zoneCUE("b.dev", "int", `{type: "A", name: "", content: "2.2.2.2", ttl: 600}`),
		}, nil)

		o := h.dnsSync()
		wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "failed", "skipped", "skipped", "skipped", "skipped", "completed")...)
		if calls := h.toolCalls(); len(calls) != 0 || api.Requests() != 0 {
			t.Errorf("ran %q and sent %d requests despite the linked zone", calls, api.Requests())
		}
	})
	for _, link := range []string{"dns", "dns/a.dev/zone.cue", "cue.mod/pkg/example.com/shared"} {
		t.Run(link, func(t *testing.T) {
			h := newHarness(t)
			outside := h.write("outside/zone.cue", zoneCUE("a.dev", "int", `{type: "A", name: "", content: "6.6.6.6", ttl: 600}`))
			path := filepath.Join(h.infra, link)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			target := outside
			if link != "dns/a.dev/zone.cue" {
				target = filepath.Dir(outside) // a package directory
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			api := h.dnsFixture(nil, nil)

			o := h.dnsSync()
			wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "failed", "skipped", "skipped", "skipped", "skipped", "completed")...)
			if calls := h.toolCalls(); len(calls) != 0 || api.Requests() != 0 {
				t.Errorf("ran %q and sent %d requests despite the symlink", calls, api.Requests())
			}
		})
	}
}

// TestDNS_DuplicateZonesNeverReachTheAPI relies on dns sync refusing
// duplicate zones before any request, which it reports with its own exit
// status: apply failed, and nothing changed.
func TestDNS_DuplicateZonesNeverReachTheAPI(t *testing.T) {
	h := newHarness(t)
	api := h.dnsFixture(map[string]string{
		"dns/one/zone.cue": zoneCUE("gunk.dev", "int", `{type: "A", name: "www", content: "1.2.3.4", ttl: 600}`),
		"dns/two/zone.cue": zoneCUE("gunk.dev", "int", `{type: "A", name: "mail", content: "5.6.7.8", ttl: 600}`),
	}, nil)

	o := h.dnsSync()
	wantRun(t, o, 1, outcomeFailed, phaseNames("completed", "completed", "skipped", "skipped", "failed", "skipped", "completed")...)
	if api.Requests() != 0 {
		t.Errorf("Porkbun received %d requests", api.Requests())
	}
}

// TestDNS_SyncFailsPartway fails the second zone's write after the first
// zone's landed: apply is uncertain, and the sync's log, named on stderr,
// shows which records it had already changed.
func TestDNS_SyncFailsPartway(t *testing.T) {
	h := newHarness(t)
	api := h.dnsFixture(twoZones, twoZoneRecords)
	api.FailOn("create b.dev")

	o := h.dnsSync()
	t.Logf("deploy printed:\n%s%s", o.stdout, o.stderr)
	wantRun(t, o, 1, outcomeUncertain, phaseNames("completed", "completed", "skipped", "skipped", "uncertain", "skipped", "completed")...)
	wantIsolated(t, h, o)
	if log := readAll(t, namedLog(t, o.stderr, "dns-sync")); !strings.Contains(log, "CREATE A @ -> 1.1.1.1") {
		t.Errorf("the sync's log does not show the write that landed:\n%s", log)
	}
}

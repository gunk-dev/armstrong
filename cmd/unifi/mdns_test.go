package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mDNS tests drive the fake's `rest/setting` mdns entry, which starts as the
// console UI's Auto leaves it: mode all, both service lists empty, and
// enabled_for projected from the networks' mdnsForwardingEnabled.

const mdnsPath = "legacy/set/setting/mdns"

// iotJoinsProxy declares seedSite's two networks as they are, except that IoT
// joins the proxy.
const iotJoinsProxy = `"networks": [
  {"name":"Default","management":"GATEWAY","enabled":true,"vlanId":1,
   "isolationEnabled":false,"internetAccessEnabled":true,"cellularBackupEnabled":false,"mdnsForwardingEnabled":false,
   "ipv4":{"hostIpAddress":"192.0.2.1","prefixLength":24,"autoScaleEnabled":false,
     "dhcp":{"mode":"SERVER","rangeStart":"192.0.2.100","rangeStop":"192.0.2.199","leaseTimeSeconds":86400}}},
  {"name":"IoT","management":"GATEWAY","enabled":true,"vlanId":20,
   "isolationEnabled":true,"internetAccessEnabled":true,"cellularBackupEnabled":false,"mdnsForwardingEnabled":true,
   "ipv4":{"hostIpAddress":"198.51.100.1","prefixLength":24,"autoScaleEnabled":false,
     "dhcp":{"mode":"SERVER","rangeStart":"198.51.100.100","rangeStop":"198.51.100.199","leaseTimeSeconds":3600}}}
]`

// newRecord is a DNS record to create: an Integration API write to put in
// the same run as an mdns change.
const newRecord = `"dnsPolicies": [
  {"type":"A_RECORD","enabled":true,"domain":"nas.example.internal","ipv4Address":"192.0.2.10","ttlSeconds":0},
  {"type":"A_RECORD","enabled":true,"domain":"new.example.internal","ipv4Address":"192.0.2.30","ttlSeconds":0}
]`

var mainPassphrase = []string{"UNIFI_WIFI_EXAMPLE_MAIN=super-secret-passphrase"}

// customEntry is a custom_services element as the console stores it.
func customEntry(address, name string) map[string]any {
	return map[string]any{"address": address, "name": name}
}

// setCustomMDNS puts the fake's proxy in custom mode with these predefined
// services and custom services.
func setCustomMDNS(f *fakeConsole, codes []string, custom ...map[string]any) {
	entries := []any{}
	for _, c := range custom {
		entries = append(entries, c)
	}
	f.setMDNS(map[string]any{"mode": "custom", "predefined_services": serviceCodes(codes...), "custom_services": entries})
}

// setLegacyAutoMDNS puts the fake's proxy in the state a console reports
// when it has not been written since its migration to 10.6.
func setLegacyAutoMDNS(f *fakeConsole) {
	f.setMDNS(map[string]any{"mode": "auto", "predefined_services": serviceCodes(mdnsCatalogue...)})
}

// setMDNSFlag sets a network's mdnsForwardingEnabled directly, and the legacy
// projection with it.
func setMDNSFlag(f *fakeConsole, network string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.coll[collNetworks].list() {
		if n["name"] == network {
			n["mdnsForwardingEnabled"] = on
		}
	}
	f.projectMDNSLocked()
}

// mdnsPost is the write expected against the fake's current mdns entry:
// the five keys, enabled_for* echoed as they stand.
func mdnsPost(f *fakeConsole, mode string, predefined, custom []any) mutation {
	m := f.mdnsSetting()
	return mutation{Method: "POST", Path: mdnsPath, Body: map[string]any{
		"mode": mode, "predefined_services": predefined, "custom_services": custom,
		"enabled_for": m["enabled_for"], "enabled_for_network_ids": m["enabled_for_network_ids"],
	}}
}

func TestExportReadsMDNS(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*fakeConsole)
		want mdns
		line string
	}{
		{"all", func(*fakeConsole) {}, mdns{Mode: "all"}, "OK     mdns proxy     all\n"},
		{"legacy auto reads as all", setLegacyAutoMDNS, mdns{Mode: "all"}, "OK     mdns proxy     all\n"},
		{"custom with labelled custom services", func(f *fakeConsole) {
			setCustomMDNS(f, []string{"printers", "apple_airPlay"},
				customEntry("_hap._tcp", "HomeKit test"), customEntry("_airplay._tcp", "Living room"))
			setMDNSFlag(f, "IoT", true)
		}, mdns{Mode: "custom", Services: []string{"apple_airPlay", "printers"}, CustomServices: []customService{
			{Name: "Living room", Address: "_airplay._tcp"}, {Name: "HomeKit test", Address: "_hap._tcp"},
		}}, "OK     mdns proxy     custom (services apple_airPlay, printers; " +
			"customServices Living room (_airplay._tcp), HomeKit test (_hap._tcp))\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeConsole(t)
			seedSite(f)
			tc.seed(f)

			exported := mustRun(t, f, "", nil, "export")
			var doc site
			if err := json.Unmarshal([]byte(exported), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.MDNS == nil || !reflect.DeepEqual(*doc.MDNS, tc.want) {
				t.Fatalf("exported mdns = %+v, want %+v", doc.MDNS, tc.want)
			}
			stdout, _, code := run(t, f, exported, mainPassphrase, "diff", "--prune")
			if code != 0 {
				t.Fatalf("export -> diff is not a no-op (exit %d):\n%s", code, stdout)
			}
			if !strings.Contains(stdout, tc.line) {
				t.Errorf("diff is missing %q:\n%s", tc.line, stdout)
			}
		})
	}
}

func TestMDNSAbsentIsNotRead(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	mustRun(t, f, `{"reservations": []}`, nil, "sync", "--prune")
	for _, req := range f.legacyRequestLog() {
		if strings.Contains(req, "setting") {
			t.Errorf("an absent mdns reached the settings: %s", req)
		}
	}

	// And mdns alone reads nothing else.
	f = newFakeConsole(t)
	seedSite(f)
	mustRun(t, f, `{"mdns": {"mode": "all"}}`, nil, "sync")
	if got, want := f.legacyRequestLog(), []string{"GET rest/setting"}; !reflect.DeepEqual(got, want) {
		t.Errorf("legacy requests = %v, want %v", got, want)
	}
}

// TestMDNSServiceChangeIsOnePost narrows the scope alongside a network joining
// the proxy: the scope is one POST with all five keys, written before the
// network update, and participation is the network's flag alone.
func TestMDNSServiceChangeIsOnePost(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	setMDNSFlag(f, "Default", true)
	iotID := f.objectNamed(collNetworks, "IoT")["id"].(string)
	wantPost := mutation{Method: "POST", Path: mdnsPath, Body: map[string]any{
		"mode":                "custom",
		"predefined_services": serviceCodes("apple_airPlay", "printers"),
		"custom_services":     []any{customEntry("_hap._tcp", "HomeKit")},
		// As read: only Default participates until IoT's update lands.
		"enabled_for": "some", "enabled_for_network_ids": []any{f.legacyNetworkIDNamed("Default")},
	}}

	desired := `{` + strings.Replace(iotJoinsProxy, `"mdnsForwardingEnabled":false`, `"mdnsForwardingEnabled":true`, 1) +
		`, "mdns": {"mode": "custom", "services": ["printers", "apple_airPlay"],
	  "customServices": [{"name": "HomeKit", "address": "_hap._tcp"}]}}`
	stdout := mustRun(t, f, desired, nil, "sync")

	const line = "UPDATE mdns proxy     custom (mode all -> custom; services +apple_airPlay +printers; " +
		"customServices +HomeKit (_hap._tcp))\n"
	if !strings.Contains(stdout, line) {
		t.Errorf("plan is missing %q:\n%s", line, stdout)
	}
	muts := f.recorded()
	if len(muts) != 2 {
		t.Fatalf("writes = %+v, want the mdns POST then the IoT network PUT", muts)
	}
	if !reflect.DeepEqual(muts[0], wantPost) {
		t.Errorf("first write = %+v\nwant %+v", muts[0], wantPost)
	}
	if muts[1].Method != "PUT" || muts[1].Path != "networks/"+iotID {
		t.Errorf("second write = %s %s, want the IoT network update", muts[1].Method, muts[1].Path)
	}
	if m := f.mdnsSetting(); m["enabled_for"] != "all" {
		t.Errorf("after IoT joined, enabled_for = %v, want all", m["enabled_for"])
	}

	stdout, _, code := run(t, f, desired, nil, "diff")
	if code != 0 || !strings.Contains(stdout, "OK     mdns proxy     custom (services printers, apple_airPlay; "+
		"customServices HomeKit (_hap._tcp))\n") {
		t.Errorf("second run is not a no-op (exit %d):\n%s", code, stdout)
	}
}

// Participation is written through the network, never through the proxy.
func TestMDNSParticipationIsTheNetworkFlag(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)

	mustRun(t, f, `{`+iotJoinsProxy+`, "mdns": {"mode": "all"}}`, nil, "sync")
	if got := legacyWrites(f); len(got) != 0 {
		t.Errorf("joining the proxy wrote to the legacy API: %+v", got)
	}
	m := f.mdnsSetting()
	if want := []any{f.legacyNetworkIDNamed("IoT")}; m["enabled_for"] != "some" || !reflect.DeepEqual(m["enabled_for_network_ids"], want) {
		t.Errorf("projection = %v %v, want some %v", m["enabled_for"], m["enabled_for_network_ids"], want)
	}
}

func TestMDNSCustomToAllSendsEmptyLists(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	setCustomMDNS(f, []string{"printers", "ssh_servers"}, customEntry("_hap._tcp", "HomeKit"))
	setMDNSFlag(f, "IoT", true)
	wantPost := mdnsPost(f, "all", []any{}, []any{})

	stdout := mustRun(t, f, `{"mdns": {"mode": "all"}}`, nil, "sync")
	if line := "UPDATE mdns proxy     all (mode custom -> all)\n"; !strings.Contains(stdout, line) {
		t.Errorf("plan is missing %q:\n%s", line, stdout)
	}
	if got := legacyWrites(f); !reflect.DeepEqual(got, []mutation{wantPost}) {
		t.Fatalf("writes = %+v\nwant %+v", got, wantPost)
	}
}

// A console still reporting the legacy auto, catalogue and all, is what an
// instance file's all asks for.
func TestMDNSLegacyAutoIsAll(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	setLegacyAutoMDNS(f)

	stdout, _, code := run(t, f, `{"mdns": {"mode": "all"}}`, nil, "diff")
	if code != 0 || !strings.Contains(stdout, "OK     mdns proxy     all\n") {
		t.Errorf("legacy auto against all is not a no-op (exit %d):\n%s", code, stdout)
	}
}

func TestMDNSPlanLines(t *testing.T) {
	seeded := []string{"apple_airPlay", "ssh_servers", "web_servers"}
	hap := customEntry("_hap._tcp", "HomeKit")
	for _, tc := range []struct {
		name string
		mdns string
		line string
		post func(*fakeConsole) mutation // nil: no write
	}{
		{"same sets in another order",
			`{"mode": "custom", "services": ["web_servers", "apple_airPlay", "ssh_servers"], "customServices": [{"name": "HomeKit", "address": "_hap._tcp"}]}`,
			"OK     mdns proxy     custom (services web_servers, apple_airPlay, ssh_servers; customServices HomeKit (_hap._tcp))", nil},
		{"services added and removed",
			`{"mode": "custom", "services": ["web_servers", "printers", "apple_airPlay"], "customServices": [{"name": "HomeKit", "address": "_hap._tcp"}]}`,
			"UPDATE mdns proxy     custom (services +printers -ssh_servers)",
			func(f *fakeConsole) mutation {
				return mdnsPost(f, "custom", serviceCodes("apple_airPlay", "printers", "web_servers"), []any{hap})
			}},
		{"custom service relabelled",
			`{"mode": "custom", "services": ["apple_airPlay", "ssh_servers", "web_servers"], "customServices": [{"name": "Home", "address": "_hap._tcp"}]}`,
			"UPDATE mdns proxy     custom (customServices +Home (_hap._tcp) -HomeKit (_hap._tcp))",
			func(f *fakeConsole) mutation {
				return mdnsPost(f, "custom", serviceCodes(seeded...), []any{customEntry("_hap._tcp", "Home")})
			}},
		{"to all", `{"mode": "all"}`, "UPDATE mdns proxy     all (mode custom -> all)",
			func(f *fakeConsole) mutation { return mdnsPost(f, "all", []any{}, []any{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeConsole(t)
			seedSite(f)
			setCustomMDNS(f, seeded, hap)
			var want []mutation
			if tc.post != nil {
				want = []mutation{tc.post(f)}
			}

			stdout := mustRun(t, f, `{"mdns": `+tc.mdns+`}`, nil, "sync")
			if !strings.Contains(stdout, tc.line+"\n") {
				t.Errorf("plan is missing %q:\n%s", tc.line, stdout)
			}
			if got := legacyWrites(f); !reflect.DeepEqual(got, want) {
				t.Errorf("writes = %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestMDNSDryRunAndDiffWriteNothing(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	desired := `{"mdns": {"mode": "custom", "services": ["printers"]}}`

	stdout := mustRun(t, f, desired, nil, "sync", "--dry-run")
	if !strings.Contains(stdout, "DRY RUN") || !strings.Contains(stdout, "UPDATE mdns proxy     custom (mode all -> custom;") {
		t.Errorf("dry run did not plan the update:\n%s", stdout)
	}
	if stdout, _, code := run(t, f, desired, nil, "diff"); code != exitChangesPending {
		t.Errorf("diff exit = %d, want %d:\n%s", code, exitChangesPending, stdout)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Fatalf("--dry-run / diff wrote to the console: %+v", muts)
	}
}

func TestMDNSUnreadableSettingIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting map[string]any
		err     string
	}{
		{"unknown mode", map[string]any{"mode": "smart"}, `mode "smart" is not one #MDNS models`},
		{"unknown enabled_for", map[string]any{"enabled_for": "custom"}, `enabled_for "custom" is not none, all or some`},
		{"custom service not an object", map[string]any{"mode": "custom", "predefined_services": serviceCodes("printers"),
			"custom_services": []any{"_hap._tcp"}}, `custom service "_hap._tcp" is not the`},
		{"custom service with unknown fields", map[string]any{"mode": "custom", "predefined_services": serviceCodes("printers"),
			"custom_services": []any{map[string]any{"address": "_hap._tcp", "name": "HomeKit", "port": 80}}},
			`custom service {"address":"_hap._tcp","name":"HomeKit","port":80} is not the`},
		{"custom service without a name", map[string]any{"mode": "custom",
			"custom_services": []any{map[string]any{"address": "_hap._tcp"}}}, `custom service {"address":"_hap._tcp"} is not the`},
		// Informational in all, but a write replaces it unread.
		{"custom service of another shape in all", map[string]any{"custom_services": []any{map[string]any{"service": "_x._tcp"}}},
			`custom service {"service":"_x._tcp"} is not the`},
		// Decode failures of the mdns object itself, before any projection.
		{"custom_services not a list", map[string]any{"custom_services": map[string]any{"name": "_hap._tcp"}},
			`parse mdns setting: json: cannot unmarshal object`},
		{"network ids not strings", map[string]any{"enabled_for_network_ids": []any{7}},
			`parse mdns setting: json: cannot unmarshal number`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeConsole(t)
			seedSite(f)
			f.setMDNS(tc.setting)

			stdout, stderr, code := run(t, f, "", nil, "export")
			var doc map[string]json.RawMessage
			if code != 0 || json.Unmarshal([]byte(stdout), &doc) != nil {
				t.Fatalf("export exited %d:\n%s", code, stderr)
			}
			if _, ok := doc["mdns"]; ok || !strings.Contains(stderr, "skipping the mdns proxy setting: ") || !strings.Contains(stderr, tc.err) {
				t.Errorf("export did not leave mdns out with a warning naming %q:\n%s", tc.err, stderr)
			}

			desired := `{` + newRecord + `, "mdns": {"mode": "custom", "services": ["printers"]}}`
			dir := t.TempDir()
			for _, args := range [][]string{{"diff"}, {"sync"}, {"sync", "--snapshot-dir", dir}} {
				_, stderr, code := run(t, f, desired, nil, args...)
				if code != 1 || !strings.Contains(stderr, tc.err) || !strings.Contains(stderr, "refusing to manage") {
					t.Errorf("%v exited %d, want 1 naming %q:\n%s", args, code, tc.err, stderr)
				}
			}
			if muts := f.recorded(); len(muts) != 0 {
				t.Errorf("wrote despite an unreadable setting: %+v", muts)
			}
			if s := snapshots(t, dir); len(s) != 0 {
				t.Errorf("wrote a snapshot: %v", s)
			}
		})
	}
}

// The planning pass read the setting fine; by the time the snapshot reads it,
// it is something the snapshot could not replay.
func TestMDNSSnapshotRefusesUnreadableSetting(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	dir := t.TempDir()
	reads := 0
	f.onLegacyRequest = func(method, rest string) {
		if method == "GET" && rest == "rest/setting" {
			if reads++; reads == 2 {
				f.setMDNS(map[string]any{"mode": "smart"})
			}
		}
	}

	_, stderr, code := run(t, f, `{"mdns": {"mode": "custom", "services": ["printers"]}}`, nil, "sync", "--snapshot-dir", dir)
	if code != 1 || !strings.Contains(stderr, `snapshot: mdns proxy mode "smart"`) {
		t.Errorf("sync exited %d, want 1 with the snapshot refusing:\n%s", code, stderr)
	}
	if reads != 2 {
		t.Errorf("rest/setting was read %d times, want 2 (plan, snapshot)", reads)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Errorf("wrote without a snapshot: %+v", muts)
	}
	if s := snapshots(t, dir); len(s) != 0 {
		t.Errorf("wrote a snapshot: %v", s)
	}
}

func TestMDNSFailedWriteAbortsTheRun(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	f.mdnsWriteRefused = true

	desired := `{` + iotJoinsProxy + `, "mdns": {"mode": "custom", "services": ["printers"]}}`
	_, stderr, code := run(t, f, desired, nil, "sync")
	if code != 1 || !strings.Contains(stderr, "update mdns proxy") || !strings.Contains(stderr, "api.err.InvalidValue") {
		t.Fatalf("sync exited %d, want 1 naming the failed mdns write:\n%s", code, stderr)
	}
	for _, m := range f.recorded() {
		if m.Path != mdnsPath {
			t.Errorf("wrote %s %s after the mdns write failed", m.Method, m.Path)
		}
	}
	if f.objectNamed(collNetworks, "IoT")["mdnsForwardingEnabled"] != false {
		t.Errorf("IoT joined the proxy although its allow-list was never written")
	}
}

func TestMDNSSnapshotRestoreRoundTrip(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	setCustomMDNS(f, []string{"printers", "apple_airPlay"}, customEntry("_hap._tcp", "HomeKit test"))
	dir := t.TempDir()
	before := mustRun(t, f, "", nil, "export")

	mustRun(t, f, `{"mdns": {"mode": "all"}}`, nil, "sync", "--snapshot-dir", dir)
	snaps := snapshots(t, dir)
	if len(snaps) != 1 {
		t.Fatalf("have %d snapshots, want 1", len(snaps))
	}
	data, err := os.ReadFile(snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	var snap site
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	want := mdns{Mode: "custom", Services: []string{"apple_airPlay", "printers"},
		CustomServices: []customService{{Name: "HomeKit test", Address: "_hap._tcp"}}}
	if snap.MDNS == nil || !reflect.DeepEqual(*snap.MDNS, want) {
		t.Fatalf("snapshot mdns = %+v, want %+v", snap.MDNS, want)
	}
	if mode := f.mdnsSetting()["mode"]; mode != "all" {
		t.Fatalf("sync left mode %v", mode)
	}

	stdout := mustRun(t, f, "", mainPassphrase, "restore", snaps[0])
	if line := "UPDATE mdns proxy     custom (mode all -> custom; services +apple_airPlay +printers; " +
		"customServices +HomeKit test (_hap._tcp))\n"; !strings.Contains(stdout, line) {
		t.Errorf("restore plan is missing %q:\n%s", line, stdout)
	}
	if after := mustRun(t, f, "", nil, "export"); after != before {
		t.Errorf("restore did not round-trip the proxy\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A snapshot whose mdns names networks predates the service-scope-only #MDNS:
// restore refuses it rather than apply it without the networks.
func TestMDNSRestoreRefusesOldShape(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	path := filepath.Join(t.TempDir(), "snapshot-old.json")
	old := `{"networks": [], "mdns": {"mode": "custom", "services": ["printers"], "networks": ["IoT"]}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := run(t, f, "", mainPassphrase, "restore", "--dry-run", path)
	if code != 1 || !strings.Contains(stderr, "mdns is not #MDNS-shaped") || !strings.Contains(stderr, `unknown field "networks"`) {
		t.Errorf("restore exited %d, want 1 refusing the mdns shape:\n%s", code, stderr)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Errorf("restore wrote: %+v", muts)
	}
}

// A run that does not manage the proxy snapshots without it, so restoring
// that snapshot leaves the proxy alone.
func TestMDNSUnmanagedSnapshotLeavesProxyAlone(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	setCustomMDNS(f, []string{"printers"})
	dir := t.TempDir()

	mustRun(t, f, `{`+newRecord+`}`, nil, "sync", "--snapshot-dir", dir)
	snaps := snapshots(t, dir)
	if len(snaps) != 1 {
		t.Fatalf("have %d snapshots, want 1", len(snaps))
	}
	data, err := os.ReadFile(snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["mdns"]; ok {
		t.Fatalf("snapshot of a run without mdns holds it:\n%s", data)
	}

	f.setMDNS(map[string]any{"mode": "all", "predefined_services": []any{}})
	n := len(f.legacyRequestLog())
	mustRun(t, f, "", mainPassphrase, "restore", snaps[0])
	for _, req := range f.legacyRequestLog()[n:] {
		if strings.Contains(req, "setting") {
			t.Errorf("restore reached the settings: %s", req)
		}
	}
	if mode := f.mdnsSetting()["mode"]; mode != "all" {
		t.Errorf("restore changed the unmanaged proxy to %v", mode)
	}
}

func TestMDNSUpdateCountsTowardsMaxChanges(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	// Two updates: the proxy and the seeded record. Neither is a deletion, so
	// --prune needs no `deletions`.
	desired := `{"mdns": {"mode": "custom", "services": ["printers"]}, "dnsPolicies": [
	  {"type":"A_RECORD","enabled":true,"domain":"nas.example.internal","ipv4Address":"192.0.2.11","ttlSeconds":0}]}`

	stdout, stderr, code := run(t, f, desired, nil, "sync", "--prune", "--max-changes", "1")
	if code != 1 || !strings.Contains(stderr, "the plan changes 2 object(s) (0 deleted, 2 updated") {
		t.Fatalf("guard did not count the mdns update (exit %d)\n%s\n%s", code, stdout, stderr)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Fatalf("a refused sync wrote to the console: %+v", muts)
	}

	stdout = mustRun(t, f, desired, nil, "sync", "--prune", "--max-changes", "2")
	if strings.Contains(stdout, "DELETE") {
		t.Errorf("the proxy was treated as a deletion:\n%s", stdout)
	}
	if mode := f.mdnsSetting()["mode"]; mode != "custom" {
		t.Errorf("sync within the limit left mode %v", mode)
	}
}

// A network update carries the zoneId the console reported, so it cannot
// detach the network from its firewall zone.
func TestNetworkUpdateKeepsZoneID(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	iot := f.objectNamed(collNetworks, "IoT")
	f.mu.Lock()
	iot["zoneId"] = "zone-iot"
	f.mu.Unlock()

	mustRun(t, f, `{`+iotJoinsProxy+`}`, nil, "sync")
	muts := f.recorded()
	if len(muts) != 1 || muts[0].Body["zoneId"] != "zone-iot" {
		t.Fatalf("writes = %+v, want one network PUT carrying zoneId", muts)
	}
	if got := f.objectNamed(collNetworks, "IoT")["zoneId"]; got != "zone-iot" {
		t.Errorf("IoT's zoneId after the update = %v", got)
	}
}

// TestCueVetMDNS pins #MDNS's cross-field rules, which only `cue vet` sees.
func TestCueVetMDNS(t *testing.T) {
	if _, err := exec.LookPath("cue"); err != nil {
		t.Skip("cue not installed")
	}
	for _, dir := range []string{"all-with-services", "custom-without-services", "missing-mode", "mode-off", "networks"} {
		cmd := exec.Command("cue", "vet", "-c", "./cmd/unifi/testdata/mdns-invalid/"+dir)
		cmd.Dir = "../.."
		out, err := cmd.CombinedOutput()
		// #MDNS holds the schema's only `_|_`, which cue reports without a path.
		if err == nil {
			t.Errorf("cue vet accepted testdata/mdns-invalid/%s", dir)
		} else if !strings.Contains(string(out), "site.mdns") && !strings.Contains(string(out), "(_|_ literal)") {
			t.Errorf("cue vet rejected testdata/mdns-invalid/%s for something other than mdns:\n%s", dir, out)
		}
	}

	cmd := exec.Command("cue", "export", "./examples/unifi", "--out", "json", "-e", "site")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cue export of the example: %v", err)
	}
	var doc site
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MDNS == nil {
		t.Fatal("the example declares no mdns")
	}
	if err := checkMDNS(doc.MDNS); err != nil {
		t.Errorf("the example's mdns fails checkMDNS: %v", err)
	}
}

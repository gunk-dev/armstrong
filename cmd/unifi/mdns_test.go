package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// mDNS tests drive the fake's `rest/setting` mdns entry, which starts as a
// stock console holds it: mode auto, every network, the whole catalogue.

const mdnsPath = "legacy/rest/setting/mdns/" + fakeMDNSID

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

// setCustomMDNS puts the fake's proxy in custom mode with these services,
// restricted to the named networks, or on every network when none are named.
func setCustomMDNS(f *fakeConsole, codes []string, networks ...string) {
	fields := map[string]any{"mode": "custom", "predefined_services": serviceCodes(codes...),
		"enabled_for": "all", "enabled_for_network_ids": []any{}}
	if len(networks) > 0 {
		ids := []any{}
		for _, n := range networks {
			ids = append(ids, f.legacyNetworkIDNamed(n))
		}
		fields["enabled_for"], fields["enabled_for_network_ids"] = "custom", ids
	}
	f.setMDNS(fields)
}

// mdnsPut is the PUT expected against the fake's current mdns entry: the
// entry minus `_id`, with fields replaced.
func mdnsPut(f *fakeConsole, fields map[string]any) mutation {
	body := f.mdnsSetting()
	delete(body, "_id")
	for k, v := range fields {
		body[k] = v
	}
	return mutation{Method: "PUT", Path: mdnsPath, Body: body}
}

func TestExportReadsMDNS(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*fakeConsole)
		want mdns
		line string
	}{
		{"auto", func(*fakeConsole) {}, mdns{Mode: "auto"}, "OK     mdns proxy     auto\n"},
		{"custom", func(f *fakeConsole) {
			setCustomMDNS(f, []string{"printers", "apple_airPlay"}, "IoT", "Default")
			f.setMDNS(map[string]any{"custom_services": []any{map[string]any{"name": "_hap._tcp"}}})
		}, mdns{Mode: "custom", Services: []string{"apple_airPlay", "printers"}, CustomServices: []string{"_hap._tcp"}, Networks: []string{"Default", "IoT"}},
			"OK     mdns proxy     custom (services apple_airPlay, printers; customServices _hap._tcp; networks Default, IoT)\n"},
		{"off", func(f *fakeConsole) { f.setMDNS(map[string]any{"mode": "off"}) }, mdns{Mode: "off"}, "OK     mdns proxy     off\n"},
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
		if strings.Contains(req, "rest/setting") {
			t.Errorf("an absent mdns reached rest/setting: %s", req)
		}
	}

	// And mdns alone reads nothing reservations need.
	f = newFakeConsole(t)
	seedSite(f)
	mustRun(t, f, `{"mdns": {"mode": "auto"}}`, nil, "sync")
	if got, want := f.legacyRequestLog(), []string{"GET rest/networkconf", "GET rest/setting"}; !reflect.DeepEqual(got, want) {
		t.Errorf("legacy requests = %v, want %v", got, want)
	}
}

// TestMDNSAutoToCustom is the change cosmo needs, made alongside a network
// joining the proxy: the allow-list must land first.
func TestMDNSAutoToCustom(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	def, iot := f.legacyNetworkIDNamed("Default"), f.legacyNetworkIDNamed("IoT")
	iotID := f.objectNamed(collNetworks, "IoT")["id"].(string)
	wantPut := mdnsPut(f, map[string]any{
		"mode": "custom", "enabled_for": "custom", "enabled_for_network_ids": []any{iot, def},
		"predefined_services": serviceCodes("apple_airPlay", "printers"),
		"custom_services":     []any{map[string]any{"name": "_hap._tcp"}},
	})

	desired := `{` + iotJoinsProxy + `, "mdns": {"mode": "custom", "services": ["printers", "apple_airPlay"],
	  "customServices": ["_hap._tcp"], "networks": ["IoT", "Default"]}}`
	stdout := mustRun(t, f, desired, nil, "sync")

	const line = "UPDATE mdns proxy     custom (mode auto -> custom; services +apple_airPlay +printers; " +
		"customServices +_hap._tcp; networks all -> IoT, Default)\n"
	if !strings.Contains(stdout, line) {
		t.Errorf("plan is missing %q:\n%s", line, stdout)
	}
	muts := f.recorded()
	if len(muts) != 2 {
		t.Fatalf("writes = %+v, want the mdns PUT then the IoT network PUT", muts)
	}
	if !reflect.DeepEqual(muts[0], wantPut) {
		t.Errorf("first write = %+v\nwant %+v", muts[0], wantPut)
	}
	if muts[1].Method != "PUT" || muts[1].Path != "networks/"+iotID {
		t.Errorf("second write = %s %s, want the IoT network update", muts[1].Method, muts[1].Path)
	}

	stdout, _, code := run(t, f, desired, nil, "diff")
	if code != 0 || !strings.Contains(stdout, "OK     mdns proxy     custom (services printers, apple_airPlay; "+
		"customServices _hap._tcp; networks IoT, Default)\n") {
		t.Errorf("second run is not a no-op (exit %d):\n%s", code, stdout)
	}
}

func TestMDNSCustomToAutoSendsListsBackAsRead(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	setCustomMDNS(f, []string{"printers", "ssh_servers"}, "IoT")
	f.setMDNS(map[string]any{"custom_services": []any{map[string]any{"name": "_hap._tcp"}}})
	wantPut := mdnsPut(f, map[string]any{"mode": "auto", "enabled_for": "all", "enabled_for_network_ids": []any{}})

	stdout := mustRun(t, f, `{"mdns": {"mode": "auto"}}`, nil, "sync")
	if line := "UPDATE mdns proxy     auto (mode custom -> auto; networks IoT -> all)\n"; !strings.Contains(stdout, line) {
		t.Errorf("plan is missing %q:\n%s", line, stdout)
	}
	if got := legacyWrites(f); !reflect.DeepEqual(got, []mutation{wantPut}) {
		t.Fatalf("writes = %+v\nwant %+v", got, wantPut)
	}
}

func TestMDNSPlanLines(t *testing.T) {
	seeded := []string{"apple_airPlay", "ssh_servers", "web_servers"}
	for _, tc := range []struct {
		name     string
		networks []string // the seeded restriction; none means all
		mdns     string
		line     string
		put      func(*fakeConsole) map[string]any // nil: no write
	}{
		{"same sets in another order", []string{"Default", "IoT"},
			`{"mode": "custom", "services": ["web_servers", "apple_airPlay", "ssh_servers"], "networks": ["IoT", "Default"]}`,
			"OK     mdns proxy     custom (services web_servers, apple_airPlay, ssh_servers; networks IoT, Default)", nil},
		{"services added and removed", []string{"Default", "IoT"},
			`{"mode": "custom", "services": ["web_servers", "printers", "apple_airPlay"], "networks": ["Default", "IoT"]}`,
			"UPDATE mdns proxy     custom (services +printers -ssh_servers)",
			func(*fakeConsole) map[string]any {
				return map[string]any{"predefined_services": serviceCodes("apple_airPlay", "printers", "web_servers")}
			}},
		{"services swapped for a custom one", []string{"Default", "IoT"},
			`{"mode": "custom", "services": ["apple_airPlay"], "customServices": ["_hap._tcp"], "networks": ["Default", "IoT"]}`,
			"UPDATE mdns proxy     custom (services -ssh_servers -web_servers; customServices +_hap._tcp)",
			func(*fakeConsole) map[string]any {
				return map[string]any{"predefined_services": serviceCodes("apple_airPlay"),
					"custom_services": []any{map[string]any{"name": "_hap._tcp"}}}
			}},
		{"networks to all", []string{"Default", "IoT"},
			`{"mode": "custom", "services": ["apple_airPlay", "ssh_servers", "web_servers"]}`,
			"UPDATE mdns proxy     custom (networks Default, IoT -> all)",
			func(*fakeConsole) map[string]any {
				return map[string]any{"enabled_for": "all", "enabled_for_network_ids": []any{}}
			}},
		{"networks narrowed", []string{"Default", "IoT"},
			`{"mode": "custom", "services": ["apple_airPlay", "ssh_servers", "web_servers"], "networks": ["Default"]}`,
			"UPDATE mdns proxy     custom (networks Default, IoT -> Default)",
			func(f *fakeConsole) map[string]any {
				return map[string]any{"enabled_for_network_ids": []any{f.legacyNetworkIDNamed("Default")}}
			}},
		{"custom to off", nil, `{"mode": "off"}`, "UPDATE mdns proxy     off (mode custom -> off)",
			func(*fakeConsole) map[string]any { return map[string]any{"mode": "off"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeConsole(t)
			seedSite(f)
			setCustomMDNS(f, seeded, tc.networks...)
			var want []mutation
			if tc.put != nil {
				want = []mutation{mdnsPut(f, tc.put(f))}
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
	if !strings.Contains(stdout, "DRY RUN") || !strings.Contains(stdout, "UPDATE mdns proxy     custom (mode auto -> custom;") {
		t.Errorf("dry run did not plan the update:\n%s", stdout)
	}
	if stdout, _, code := run(t, f, desired, nil, "diff"); code != exitChangesPending {
		t.Errorf("diff exit = %d, want %d:\n%s", code, exitChangesPending, stdout)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Fatalf("--dry-run / diff wrote to the console: %+v", muts)
	}
}

// The proxy runs before networks are reconciled, so a network it names must
// already exist — even one this same file creates.
func TestMDNSNetworkReferencesAreCheckedBeforeAnyWrite(t *testing.T) {
	const defaultOnly = `"networks": [{"name":"Default","management":"GATEWAY","enabled":true,"vlanId":1,
	  "isolationEnabled":false,"internetAccessEnabled":true,"cellularBackupEnabled":false,"mdnsForwardingEnabled":false,
	  "ipv4":{"hostIpAddress":"192.0.2.1","prefixLength":24,"autoScaleEnabled":false,
	    "dhcp":{"mode":"SERVER","rangeStart":"192.0.2.100","rangeStop":"192.0.2.199","leaseTimeSeconds":86400}}}]`
	for _, tc := range []struct {
		name, desired, err string
	}{
		{"unknown", `{` + newRecord + `, "mdns": {"mode": "custom", "services": ["printers"], "networks": ["Nope"]}}`,
			`mdns proxy references unknown network "Nope"; create it in a sync of its own first`},
		{"declared but not on the console", `{"networks": [{"name":"Media","enabled":true,"vlanId":30}], ` + newRecord +
			`, "mdns": {"mode": "custom", "services": ["printers"], "networks": ["Media"]}}`,
			`mdns proxy references unknown network "Media"; create it in a sync of its own first`},
		{"missing from the networks section", `{` + defaultOnly + `, ` + newRecord +
			`, "mdns": {"mode": "custom", "services": ["printers"], "networks": ["IoT"]}}`,
			`mdns proxy references network "IoT", which the networks section does not declare`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeConsole(t)
			seedSite(f)
			for _, args := range [][]string{{"sync", "--dry-run"}, {"diff"}, {"sync"}} {
				_, stderr, code := run(t, f, tc.desired, nil, args...)
				if code != 1 || !strings.Contains(stderr, tc.err) {
					t.Errorf("%v exited %d, want 1 with %q:\n%s", args, code, tc.err, stderr)
				}
			}
			if muts := f.recorded(); len(muts) != 0 {
				t.Errorf("wrote before refusing: %+v", muts)
			}
		})
	}
}

func TestMDNSUnreadableSettingIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setting map[string]any
		err     string
	}{
		{"unknown mode", map[string]any{"mode": "smart"}, `mode "smart" is not one #MDNS models`},
		{"unknown enabled_for", map[string]any{"enabled_for": "selected", "enabled_for_network_ids": []any{"legacy-networks-002"}},
			`enabled_for "selected" is not one #MDNS models`},
		{"custom service not an object", map[string]any{"mode": "custom", "predefined_services": serviceCodes("printers"),
			"custom_services": []any{"_hap._tcp"}}, `custom service "_hap._tcp" is not the`},
		{"custom service with unknown fields", map[string]any{"mode": "custom", "predefined_services": serviceCodes("printers"),
			"custom_services": []any{map[string]any{"name": "_hap._tcp", "port": 80}}}, `custom service {"name":"_hap._tcp","port":80} is not the`},
		// Ignored by the console in these modes, but a write to custom would
		// replace them unread.
		{"custom service of another shape in auto", map[string]any{"custom_services": []any{map[string]any{"service": "_x._tcp"}}},
			`custom service {"service":"_x._tcp"} is not the`},
		{"custom service of another shape in off", map[string]any{"mode": "off", "custom_services": []any{map[string]any{"service": "_x._tcp"}}},
			`custom service {"service":"_x._tcp"} is not the`},
		{"network naming nothing", map[string]any{"enabled_for": "custom", "enabled_for_network_ids": []any{"5f00000000000000000000ff"}},
			`network "5f00000000000000000000ff" is not in legacy rest/networkconf`},
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

			// custom: the desired state whose write replaces everything read.
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

	_, stderr, code := run(t, f, `{"mdns": {"mode": "off"}}`, nil, "sync", "--snapshot-dir", dir)
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
	f.mdnsPutMissing = true

	desired := `{` + iotJoinsProxy + `, "mdns": {"mode": "custom", "services": ["printers"]}}`
	_, stderr, code := run(t, f, desired, nil, "sync")
	if code != 1 || !strings.Contains(stderr, "update mdns proxy") || !strings.Contains(stderr, "api.err.NotFound") {
		t.Fatalf("sync exited %d, want 1 naming the failed mdns write:\n%s", code, stderr)
	}
	for _, m := range f.recorded() {
		if m.Path != mdnsPath {
			t.Errorf("wrote %s %s after the mdns PUT failed", m.Method, m.Path)
		}
	}
	if f.objectNamed(collNetworks, "IoT")["mdnsForwardingEnabled"] != false {
		t.Errorf("IoT joined the proxy although its allow-list was never written")
	}
}

func TestMDNSSnapshotRestoreRoundTrip(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	setCustomMDNS(f, []string{"printers", "apple_airPlay"}, "IoT")
	dir := t.TempDir()
	before := mustRun(t, f, "", nil, "export")

	mustRun(t, f, `{"mdns": {"mode": "auto"}}`, nil, "sync", "--snapshot-dir", dir)
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
	want := mdns{Mode: "custom", Services: []string{"apple_airPlay", "printers"}, Networks: []string{"IoT"}}
	if snap.MDNS == nil || !reflect.DeepEqual(*snap.MDNS, want) {
		t.Fatalf("snapshot mdns = %+v, want %+v", snap.MDNS, want)
	}
	if mode := f.mdnsSetting()["mode"]; mode != "auto" {
		t.Fatalf("sync left mode %v", mode)
	}

	stdout := mustRun(t, f, "", mainPassphrase, "restore", snaps[0])
	if line := "UPDATE mdns proxy     custom (mode auto -> custom; services +apple_airPlay +printers; networks all -> IoT)\n"; !strings.Contains(stdout, line) {
		t.Errorf("restore plan is missing %q:\n%s", line, stdout)
	}
	if after := mustRun(t, f, "", nil, "export"); after != before {
		t.Errorf("restore did not round-trip the proxy\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// A run that does not manage the proxy snapshots without it, so restoring
// that snapshot leaves the proxy alone.
func TestMDNSUnmanagedSnapshotLeavesProxyAlone(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	setCustomMDNS(f, []string{"printers"}, "IoT")
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

	f.setMDNS(map[string]any{"mode": "off", "enabled_for": "all", "enabled_for_network_ids": []any{}})
	n := len(f.legacyRequestLog())
	mustRun(t, f, "", mainPassphrase, "restore", snaps[0])
	for _, req := range f.legacyRequestLog()[n:] {
		if strings.Contains(req, "rest/setting") {
			t.Errorf("restore reached rest/setting: %s", req)
		}
	}
	if mode := f.mdnsSetting()["mode"]; mode != "off" {
		t.Errorf("restore changed the unmanaged proxy to %v", mode)
	}
}

func TestMDNSUpdateCountsTowardsMaxChanges(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	// Two updates: the proxy and the seeded record. Neither is a deletion, so
	// --prune needs no `deletions`.
	desired := `{"mdns": {"mode": "off"}, "dnsPolicies": [
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
	if mode := f.mdnsSetting()["mode"]; mode != "off" {
		t.Errorf("sync within the limit left mode %v", mode)
	}
}

// TestCueVetMDNS pins #MDNS's cross-field rules, which only `cue vet` sees.
func TestCueVetMDNS(t *testing.T) {
	if _, err := exec.LookPath("cue"); err != nil {
		t.Skip("cue not installed")
	}
	for _, dir := range []string{"auto-with-services", "custom-without-services", "empty-networks", "missing-mode"} {
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

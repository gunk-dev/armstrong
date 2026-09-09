package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// unifiBin is the real `unifi` binary, built once and driven over a pipe
// against the fake console. Going through the binary rather than calling
// reconcile() directly is what makes these tests cover flag parsing, stdin
// handling and — for `diff` — the exit code CI depends on.
var unifiBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "unifi-bin")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	unifiBin = filepath.Join(dir, "unifi")
	build := exec.Command("go", "build", "-o", unifiBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		panic("building unifi: " + err.Error() + "\n" + string(out))
	}
	os.Exit(m.Run())
}

// run invokes the binary and returns stdout, stderr and the exit code.
func run(t *testing.T, f *fakeConsole, stdin string, extraEnv []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(unifiBin, args...)
	cmd.Env = append(append(os.Environ(), f.env()...), extraEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running %v: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// mustRun fails the test unless the command exits 0.
func mustRun(t *testing.T, f *fakeConsole, stdin string, extraEnv []string, args ...string) string {
	t.Helper()
	stdout, stderr, code := run(t, f, stdin, extraEnv, args...)
	if code != 0 {
		t.Fatalf("unifi %v exited %d\nstdout:\n%s\nstderr:\n%s", args, code, stdout, stderr)
	}
	return stdout
}

// seedSite fills a fake console with a starting state: the console's own
// default network and one user-created network, SSID and DNS record.
func seedSite(f *fakeConsole) {
	f.seed(collNetworks, originSystem, map[string]any{
		"name": "Default", "management": "GATEWAY", "enabled": true,
		"vlanId": 1, "default": true,
		"isolationEnabled": false, "internetAccessEnabled": true,
		"cellularBackupEnabled": false, "mdnsForwardingEnabled": false,
		"ipv4Configuration": map[string]any{
			"hostIpAddress": "192.0.2.1", "prefixLength": 24, "autoScaleEnabled": false,
			"dhcpConfiguration": map[string]any{
				"mode":             "SERVER",
				"ipAddressRange":   map[string]any{"start": "192.0.2.100", "stop": "192.0.2.199"},
				"leaseTimeSeconds": 86400,
			},
		},
	})
	f.seed(collNetworks, originUser, map[string]any{
		"name": "IoT", "management": "GATEWAY", "enabled": true,
		"vlanId": 20, "default": false,
		"isolationEnabled": true, "internetAccessEnabled": true,
		"cellularBackupEnabled": false, "mdnsForwardingEnabled": false,
		"ipv4Configuration": map[string]any{
			"hostIpAddress": "198.51.100.1", "prefixLength": 24, "autoScaleEnabled": false,
			"dhcpConfiguration": map[string]any{
				"mode":             "SERVER",
				"ipAddressRange":   map[string]any{"start": "198.51.100.100", "stop": "198.51.100.199"},
				"leaseTimeSeconds": 3600,
			},
		},
	})
	f.seed(collWiFi, originUser, map[string]any{
		"type": "STANDARD", "name": "example-main", "enabled": true,
		"network":                    map[string]any{"type": "NATIVE"},
		"securityConfiguration":      map[string]any{"type": "WPA2_PERSONAL", "passphrase": "super-secret-passphrase"},
		"broadcastingFrequenciesGHz": []any{2.4, 5.0},
		"clientIsolationEnabled":     false, "hideName": false,
		"multicastToUnicastConversionEnabled": true, "uapsdEnabled": false,
	})
	f.seed(collDNS, originUser, map[string]any{
		"type": "A_RECORD", "enabled": true,
		"domain": "nas.example.internal", "ipv4Address": "192.0.2.10", "ttlSeconds": 0,
	})
}

// TestExportDiffRoundTrip is the core contract: whatever `export` prints must
// describe the live site precisely enough that feeding it back to `diff`
// reports no work. It catches any field the exporter drops or renames.
func TestExportDiffRoundTrip(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)

	exported := mustRun(t, f, "", nil, "export")

	var doc site
	if err := json.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatalf("export output is not #Site-shaped JSON: %v\n%s", err, exported)
	}
	if len(doc.Networks) != 2 || len(doc.WiFi) != 1 || len(doc.DNSPolicies) != 1 {
		t.Fatalf("export lost objects: %d networks, %d wifi, %d dns", len(doc.Networks), len(doc.WiFi), len(doc.DNSPolicies))
	}
	if got := doc.Networks[1].IPv4.DHCP.RangeStop; got != "198.51.100.199" {
		t.Errorf("export flattened the DHCP range wrongly: got %q", got)
	}

	// The exporter substitutes an env var name for the passphrase, so the
	// round trip needs that variable set.
	env := []string{"UNIFI_WIFI_EXAMPLE_MAIN=super-secret-passphrase"}
	stdout, _, code := run(t, f, exported, env, "diff")
	if code != 0 {
		t.Fatalf("diff of exported state wanted exit 0, got %d:\n%s", code, stdout)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line != "" && !strings.HasPrefix(line, "OK") && !strings.HasPrefix(line, "DRY RUN") {
			t.Errorf("export→diff round trip is not a no-op: %q", line)
		}
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Errorf("export and diff mutated the console: %+v", muts)
	}
}

// TestDiffReportsPlanAndExitsNonZero covers the CI gate: a plan is printed and
// the exit code is 2, distinct from the 1 an outright failure returns.
func TestDiffReportsPlanAndExitsNonZero(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)

	// Rename IoT's SSID and drop the DNS record from the desired state.
	desired := `{
	  "networks": [{"name":"IoT","management":"GATEWAY","enabled":true,"vlanId":30,
	    "isolationEnabled":true,"internetAccessEnabled":true,
	    "cellularBackupEnabled":false,"mdnsForwardingEnabled":false}],
	  "firewallZones": [], "wifi": [], "firewallPolicies": [], "dnsPolicies": []
	}`

	stdout, stderr, code := run(t, f, desired, nil, "diff")
	if code != exitChangesPending {
		t.Fatalf("diff wanted exit %d, got %d\nstdout:\n%s\nstderr:\n%s", exitChangesPending, code, stdout, stderr)
	}
	if !strings.Contains(stdout, "UPDATE") || !strings.Contains(stdout, "vlanId") {
		t.Errorf("plan does not explain the change:\n%s", stdout)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Errorf("diff mutated the console: %+v", muts)
	}
}

// TestSyncConvergesAndIsIdempotent runs the example instance in, then runs it
// again: the second run must be a no-op, which is what makes a timer-driven
// sync safe to leave running.
func TestSyncConvergesAndIsIdempotent(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	desired := exampleSiteJSON(t)
	env := []string{"UNIFI_WIFI_MAIN=main-pass", "UNIFI_WIFI_IOT=iot-pass"}

	first := mustRun(t, f, desired, env, "sync")
	if !strings.Contains(first, "CREATE") {
		t.Errorf("first sync created nothing:\n%s", first)
	}
	if got, want := f.names(collWiFi), []string{"example-iot", "example-main"}; !equalStrings(got, want) {
		t.Errorf("wifi after sync = %v, want %v", got, want)
	}
	if got, want := f.names(collZones), []string{"internal", "iot"}; !equalStrings(got, want) {
		t.Errorf("zones after sync = %v, want %v", got, want)
	}

	// The new SSID must carry the passphrase from the environment, not the
	// variable name.
	iot := f.objectNamed(collWiFi, "example-iot")
	sec, _ := iot["securityConfiguration"].(map[string]any)
	if sec["passphrase"] != "iot-pass" {
		t.Errorf("wifi passphrase not resolved from the environment: %v", sec["passphrase"])
	}

	before := len(f.recorded())
	second := mustRun(t, f, desired, env, "sync")
	if after := len(f.recorded()); after != before {
		t.Errorf("second sync made %d further writes; want none:\n%s", after-before, second)
	}
	if strings.Contains(second, "CREATE") || strings.Contains(second, "UPDATE") {
		t.Errorf("second sync was not a no-op:\n%s", second)
	}
}

// TestRecordsWithoutTTLDoNotFlap: the console defaults ttlSeconds on every DNS
// record, including the types whose schema has no TTL field. Without
// normalisation the desired (absent) and actual (0) values differ and every
// sync would rewrite the record forever.
func TestRecordsWithoutTTLDoNotFlap(t *testing.T) {
	f := newFakeConsole(t)
	desired := `{"networks":[],"firewallZones":[],"wifi":[],"firewallPolicies":[],
	  "dnsPolicies":[
	    {"type":"TXT_RECORD","enabled":true,"domain":"example.internal","text":"hello"},
	    {"type":"MX_RECORD","enabled":true,"domain":"example.internal",
	     "mailServerDomain":"mail.example.internal","priority":10}
	  ]}`

	if out := mustRun(t, f, desired, nil, "sync"); !strings.Contains(out, "CREATE") {
		t.Fatalf("first sync created nothing:\n%s", out)
	}

	before := len(f.recorded())
	second := mustRun(t, f, desired, nil, "sync")
	if after := len(f.recorded()); after != before {
		t.Errorf("second sync rewrote %d record(s); TTL defaulting is not normalised:\n%s", after-before, second)
	}

	// And `diff` must agree that there is nothing to do.
	if _, _, code := run(t, f, desired, nil, "diff"); code != 0 {
		t.Errorf("diff still reports changes after a converged sync (exit %d)", code)
	}
}

// TestSyncDryRunMakesNoMutations is the guarantee the --dry-run flag exists
// for: a full plan, including creates, with nothing written.
func TestSyncDryRunMakesNoMutations(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)

	stdout := mustRun(t, f, exampleSiteJSON(t),
		[]string{"UNIFI_WIFI_MAIN=main-pass", "UNIFI_WIFI_IOT=iot-pass"},
		"sync", "--prune", "--dry-run")

	if !strings.Contains(stdout, "DRY RUN") || !strings.Contains(stdout, "CREATE") {
		t.Errorf("dry run did not print a plan:\n%s", stdout)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Fatalf("--dry-run wrote to the console: %+v", muts)
	}
}

// TestPruneSkipsSystemDefined is the safety rule that matters most: --prune
// may remove user objects the instance file dropped, but the console's own
// SYSTEM_DEFINED objects must survive.
func TestPruneSkipsSystemDefined(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	// A second system network, plus a user SSID nobody declares.
	f.seed(collNetworks, originSystem, map[string]any{
		"name": "Guest-System", "management": "GATEWAY", "enabled": true, "vlanId": 40,
	})
	f.seed(collWiFi, originUser, map[string]any{
		"type": "STANDARD", "name": "stale-ssid", "enabled": true,
		"network":                    map[string]any{"type": "NATIVE"},
		"securityConfiguration":      map[string]any{"type": "WPA2_PERSONAL", "passphrase": "x"},
		"broadcastingFrequenciesGHz": []any{2.4},
	})

	// Desired state keeps only the IoT network and declares one SSID.
	desired := `{
	  "networks": [{"name":"IoT","management":"GATEWAY","enabled":true,"vlanId":20,
	    "isolationEnabled":true,"internetAccessEnabled":true,
	    "cellularBackupEnabled":false,"mdnsForwardingEnabled":false,
	    "ipv4":{"hostIpAddress":"198.51.100.1","prefixLength":24,"autoScaleEnabled":false,
	      "dhcp":{"mode":"SERVER","rangeStart":"198.51.100.100","rangeStop":"198.51.100.199","leaseTimeSeconds":3600}}}],
	  "firewallZones": [],
	  "wifi": [{"name":"example-main","enabled":true,"network":"NATIVE",
	    "security":{"type":"WPA2_PERSONAL","passphraseEnv":"UNIFI_WIFI_MAIN"},
	    "bands":[2.4,5],"clientIsolationEnabled":false,"hideName":false,
	    "multicastToUnicastConversionEnabled":true,"uapsdEnabled":false}],
	  "firewallPolicies": [], "dnsPolicies": []
	}`

	stdout := mustRun(t, f, desired, []string{"UNIFI_WIFI_MAIN=super-secret-passphrase"}, "sync", "--prune")

	if got, want := f.names(collNetworks), []string{"Default", "Guest-System", "IoT"}; !equalStrings(got, want) {
		t.Errorf("networks after prune = %v, want %v (SYSTEM_DEFINED must survive)\n%s", got, want, stdout)
	}
	if got, want := f.names(collWiFi), []string{"example-main"}; !equalStrings(got, want) {
		t.Errorf("wifi after prune = %v, want %v", got, want)
	}
	// dnsPolicies is empty in the input, so the undeclared record is left alone.
	if f.objectNamed(collDNS, "") == nil && len(f.names(collDNS)) != 0 {
		t.Error("unexpected dns state")
	}
	for _, m := range f.recorded() {
		if m.Method == "DELETE" && strings.HasPrefix(m.Path, collDNS) {
			t.Errorf("pruned a dns policy even though the input declared none: %+v", m)
		}
	}
}

// TestPruneLeavesUndeclaredTypesAlone guards the second prune rule: an
// instance file that simply omits a resource type must not wipe it.
func TestPruneLeavesUndeclaredTypesAlone(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)

	// Every list empty — as an instance file that forgot them would export.
	empty := `{"networks":[],"firewallZones":[],"wifi":[],"firewallPolicies":[],"dnsPolicies":[]}`
	mustRun(t, f, empty, nil, "sync", "--prune")

	if got, want := f.names(collNetworks), []string{"Default", "IoT"}; !equalStrings(got, want) {
		t.Errorf("networks = %v, want %v; an empty list must not prune", got, want)
	}
	if got, want := f.names(collWiFi), []string{"example-main"}; !equalStrings(got, want) {
		t.Errorf("wifi = %v, want %v; an empty list must not prune", got, want)
	}
	for _, m := range f.recorded() {
		if m.Method == "DELETE" {
			t.Errorf("empty input deleted something: %+v", m)
		}
	}
}

// TestUpdateTargetsTheRightID checks name-keyed matching: an update must PUT to
// the id of the object with that name, not to whichever came back first.
func TestUpdateTargetsTheRightID(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	iotID := f.objectNamed(collNetworks, "IoT")["id"].(string)

	desired := `{
	  "networks": [{"name":"IoT","management":"GATEWAY","enabled":false,"vlanId":20,
	    "isolationEnabled":true,"internetAccessEnabled":true,
	    "cellularBackupEnabled":false,"mdnsForwardingEnabled":false,
	    "ipv4":{"hostIpAddress":"198.51.100.1","prefixLength":24,"autoScaleEnabled":false,
	      "dhcp":{"mode":"SERVER","rangeStart":"198.51.100.100","rangeStop":"198.51.100.199","leaseTimeSeconds":3600}}}],
	  "firewallZones": [], "wifi": [], "firewallPolicies": [], "dnsPolicies": []
	}`
	mustRun(t, f, desired, nil, "sync")

	var puts []mutation
	for _, m := range f.recorded() {
		if m.Method == "PUT" {
			puts = append(puts, m)
		}
	}
	if len(puts) != 1 {
		t.Fatalf("want exactly one PUT, got %+v", puts)
	}
	if want := collNetworks + "/" + iotID; puts[0].Path != want {
		t.Errorf("updated %q, want %q", puts[0].Path, want)
	}
	if updated := f.get(collNetworks, iotID); updated["enabled"] != false {
		t.Errorf("IoT still enabled after sync: %v", updated["enabled"])
	}
	// The untouched system network must not have been rewritten.
	if defaultNet := f.objectNamed(collNetworks, "Default"); defaultNet["vlanId"] != 1 {
		t.Errorf("the Default network was disturbed: %v", defaultNet)
	}
}

// TestSystemDefinedIsUpdatedInPlace: the console's own default LAN can be
// managed, it just cannot be deleted.
func TestSystemDefinedIsUpdatedInPlace(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	defaultID := f.objectNamed(collNetworks, "Default")["id"].(string)

	desired := `{
	  "networks": [{"name":"Default","management":"GATEWAY","enabled":true,"vlanId":1,
	    "isolationEnabled":false,"internetAccessEnabled":true,
	    "cellularBackupEnabled":false,"mdnsForwardingEnabled":true,
	    "ipv4":{"hostIpAddress":"192.0.2.1","prefixLength":24,"autoScaleEnabled":false,
	      "dhcp":{"mode":"SERVER","rangeStart":"192.0.2.100","rangeStop":"192.0.2.199","leaseTimeSeconds":86400}}}],
	  "firewallZones": [], "wifi": [], "firewallPolicies": [], "dnsPolicies": []
	}`
	stdout := mustRun(t, f, desired, nil, "sync", "--prune")

	if !strings.Contains(stdout, "mdnsForwardingEnabled") {
		t.Errorf("plan did not name the changed field:\n%s", stdout)
	}
	if got := f.get(collNetworks, defaultID); got == nil || got["mdnsForwardingEnabled"] != true {
		t.Errorf("SYSTEM_DEFINED network was not updated in place: %v", got)
	}
}

// TestLegacyFirewallIsSkipped: on a console still running the legacy firewall
// both endpoints answer 400, which must be reported as a skip rather than
// failing the whole run.
func TestLegacyFirewallIsSkipped(t *testing.T) {
	f := newFakeConsole(t)
	f.zbfConfigured = false
	seedSite(f)

	stdout := mustRun(t, f, exampleSiteJSON(t),
		[]string{"UNIFI_WIFI_MAIN=main-pass", "UNIFI_WIFI_IOT=iot-pass"}, "sync")

	if !strings.Contains(stdout, "SKIP") || !strings.Contains(stdout, "not configured") {
		t.Errorf("legacy firewall was not reported as skipped:\n%s", stdout)
	}
	// The rest of the site must still converge.
	if got, want := f.names(collWiFi), []string{"example-iot", "example-main"}; !equalStrings(got, want) {
		t.Errorf("wifi = %v, want %v; a skipped firewall must not stop the sync", got, want)
	}
}

// TestSkippedFirewallCountsAsDrift pins the documented consequence of the skip:
// firewall rules an instance file declares but the console cannot accept are
// drift, so diff keeps reporting changes rather than claiming the site is
// converged. Declaring no firewall objects leaves diff clean.
func TestSkippedFirewallCountsAsDrift(t *testing.T) {
	f := newFakeConsole(t)
	f.zbfConfigured = false
	seedSite(f)

	withFirewall := exampleSiteJSON(t)
	env := []string{"UNIFI_WIFI_MAIN=main-pass", "UNIFI_WIFI_IOT=iot-pass"}
	mustRun(t, f, withFirewall, env, "sync")

	// Everything except the firewall is now converged, but the declared zones
	// and policies still cannot be applied.
	stdout, _, code := run(t, f, withFirewall, env, "diff")
	if code != exitChangesPending {
		t.Errorf("diff exited %d; a permanently skipped firewall must still read as drift:\n%s", code, stdout)
	}

	// The same site without firewall objects has nothing left to reconcile.
	noFirewall := strings.NewReplacer(
		`"firewallZones"`, `"_firewallZones"`,
		`"firewallPolicies"`, `"_firewallPolicies"`,
	).Replace(withFirewall)
	if stdout, _, code := run(t, f, noFirewall, env, "diff"); code != 0 {
		t.Errorf("diff exited %d with no firewall objects declared:\n%s", code, stdout)
	}
}

// TestBadAPIKeyFailsLoudly: an outright auth failure must exit non-zero rather
// than quietly producing an empty site.
func TestBadAPIKeyFailsLoudly(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)

	_, stderr, code := run(t, f, "", []string{"UNIFI_API_KEY=wrong-key"}, "export")
	if code != 1 {
		t.Fatalf("bad key wanted exit 1, got %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "401") {
		t.Errorf("error does not mention the 401: %s", stderr)
	}
}

// TestOnlyTheNotConfiguredCodeIsTreatedAsUnavailable is the counterpart to
// TestLegacyFirewallIsSkipped. The skip is keyed on one specific error code;
// any other firewall failure — an expired key, a console mid-upgrade — has to
// surface, or a sync would silently stop managing the firewall.
func TestOnlyTheNotConfiguredCodeIsTreatedAsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault fault
	}{
		{"unauthorized", fault{401, "api.unauthorized", "Unauthorized"}},
		{"server error", fault{500, "api.internal-error", "Internal Server Error"}},
		{"different 400", fault{400, "api.firewall.some-other-problem", "Nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeConsole(t)
			f.firewallFault = &tc.fault
			seedSite(f)

			stdout, stderr, code := run(t, f, exampleSiteJSON(t),
				[]string{"UNIFI_WIFI_MAIN=main-pass", "UNIFI_WIFI_IOT=iot-pass"}, "sync")
			if code == 0 {
				t.Fatalf("a %d from the firewall endpoint was swallowed\nstdout:\n%s", tc.fault.status, stdout)
			}
			if strings.Contains(stdout, "SKIP") {
				t.Errorf("reported as a skip rather than an error:\n%s", stdout)
			}
			if !strings.Contains(stderr, "firewall") {
				t.Errorf("error does not mention the firewall: %s", stderr)
			}
		})
	}
}

// TestSecretsAreNeverPrinted: the passphrase the API hands back on GET, and
// the API key itself, must not reach stdout or stderr in any command.
func TestSecretsAreNeverPrinted(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	const passphrase = "super-secret-passphrase"

	// Changing the passphrase forces the diff path to compare it.
	desired := `{
	  "networks": [], "firewallZones": [],
	  "wifi": [{"name":"example-main","enabled":true,"network":"NATIVE",
	    "security":{"type":"WPA2_PERSONAL","passphraseEnv":"UNIFI_WIFI_MAIN"},
	    "bands":[2.4,5],"clientIsolationEnabled":false,"hideName":false,
	    "multicastToUnicastConversionEnabled":true,"uapsdEnabled":false}],
	  "firewallPolicies": [], "dnsPolicies": []
	}`
	env := []string{"UNIFI_WIFI_MAIN=a-brand-new-passphrase"}

	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{"export", []string{"export"}, ""},
		{"diff", []string{"diff"}, desired},
		{"sync", []string{"sync"}, desired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, _ := run(t, f, tc.stdin, env, tc.args...)
			for _, secret := range []string{passphrase, "a-brand-new-passphrase", f.apiKey} {
				if strings.Contains(stdout, secret) {
					t.Errorf("%s leaked a secret on stdout:\n%s", tc.name, stdout)
				}
				if strings.Contains(stderr, secret) {
					t.Errorf("%s leaked a secret on stderr:\n%s", tc.name, stderr)
				}
			}
		})
	}
}

// TestAPIErrorsAreRedacted: when a write fails, the console can quote the
// payload it rejected — which for a wifi update contains the passphrase. That
// response ends up in the error message, so it must be redacted before it is
// printed.
func TestAPIErrorsAreRedacted(t *testing.T) {
	f := newFakeConsole(t)
	f.wifiPutFault = true
	seedSite(f)

	const newPassphrase = "a-brand-new-passphrase"
	desired := `{
	  "networks": [], "firewallZones": [],
	  "wifi": [{"name":"example-main","enabled":true,"network":"NATIVE",
	    "security":{"type":"WPA2_PERSONAL","passphraseEnv":"UNIFI_WIFI_MAIN"},
	    "bands":[2.4,5],"clientIsolationEnabled":false,"hideName":false,
	    "multicastToUnicastConversionEnabled":true,"uapsdEnabled":false}],
	  "firewallPolicies": [], "dnsPolicies": []
	}`

	stdout, stderr, code := run(t, f, desired, []string{"UNIFI_WIFI_MAIN=" + newPassphrase}, "sync")
	if code == 0 {
		t.Fatalf("expected the rejected update to fail the sync")
	}
	if !strings.Contains(stderr, "rejected payload") {
		t.Fatalf("test did not reach the error path; stderr: %s", stderr)
	}
	for _, out := range []string{stdout, stderr} {
		if strings.Contains(out, newPassphrase) {
			t.Errorf("passphrase survived redaction in an API error:\n%s", out)
		}
	}
	if !strings.Contains(stderr, "[REDACTED]") {
		t.Errorf("error was not redacted at all: %s", stderr)
	}
}

// TestPagingIsFollowed: the API caps a page at 200, so a site with more
// objects than that must still be read in full.
func TestPagingIsFollowed(t *testing.T) {
	f := newFakeConsole(t)
	const total = pageLimit + 37
	for i := range total {
		f.seed(collDNS, originUser, map[string]any{
			"type": "A_RECORD", "enabled": true,
			"domain":      "host" + itoa(i) + ".example.internal",
			"ipv4Address": "192.0.2.10", "ttlSeconds": 0,
		})
	}

	var doc site
	if err := json.Unmarshal([]byte(mustRun(t, f, "", nil, "export")), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.DNSPolicies) != total {
		t.Errorf("export returned %d dns policies, want %d — paging is not followed", len(doc.DNSPolicies), total)
	}
}

// TestMissingConfigFailsClearly: no console details, no silent no-op.
func TestMissingConfigFailsClearly(t *testing.T) {
	cmd := exec.Command(unifiBin, "export")
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure without UNIFI_URL, got:\n%s", out)
	}
	if !strings.Contains(string(out), "UNIFI_URL") {
		t.Errorf("error does not name the missing variable: %s", out)
	}
}

// ------------------------------------------------------------------ helpers

// exampleSiteJSON is examples/unifi/site.cue rendered to JSON. It is generated
// with `cue export` when the cue binary is available, and otherwise falls back
// to an equivalent literal so the suite still runs in a bare Go environment.
func exampleSiteJSON(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("cue"); err == nil {
		cmd := exec.Command("cue", "export", "./examples/unifi", "--out", "json", "-e", "site")
		cmd.Dir = "../.."
		out, err := cmd.Output()
		if err == nil {
			return string(out)
		}
		t.Logf("cue export failed (%v); using the built-in copy of the example", err)
	}
	return exampleSiteFallback
}

const exampleSiteFallback = `{
  "networks": [
    {"name":"Default","management":"GATEWAY","enabled":true,"vlanId":1,
     "isolationEnabled":false,"internetAccessEnabled":true,
     "cellularBackupEnabled":false,"mdnsForwardingEnabled":false,
     "ipv4":{"hostIpAddress":"192.0.2.1","prefixLength":24,"autoScaleEnabled":false,
       "dhcp":{"mode":"SERVER","rangeStart":"192.0.2.100","rangeStop":"192.0.2.199","leaseTimeSeconds":86400}}},
    {"name":"IoT","management":"GATEWAY","enabled":true,"vlanId":20,
     "isolationEnabled":true,"internetAccessEnabled":true,
     "cellularBackupEnabled":false,"mdnsForwardingEnabled":false,
     "ipv4":{"hostIpAddress":"198.51.100.1","prefixLength":24,"autoScaleEnabled":false,
       "dhcp":{"mode":"SERVER","rangeStart":"198.51.100.100","rangeStop":"198.51.100.199",
         "leaseTimeSeconds":3600,"dnsServers":["203.0.113.53"]}}}
  ],
  "firewallZones": [
    {"name":"internal","networks":["Default"]},
    {"name":"iot","networks":["IoT"]}
  ],
  "wifi": [
    {"name":"example-main","enabled":true,"network":"Default",
     "security":{"type":"WPA2_PERSONAL","passphraseEnv":"UNIFI_WIFI_MAIN"},
     "bands":[2.4,5],"clientIsolationEnabled":false,"hideName":false,
     "multicastToUnicastConversionEnabled":true,"uapsdEnabled":false},
    {"name":"example-iot","enabled":true,"network":"IoT",
     "security":{"type":"WPA2_PERSONAL","passphraseEnv":"UNIFI_WIFI_IOT"},
     "bands":[2.4],"clientIsolationEnabled":true,"hideName":true,
     "multicastToUnicastConversionEnabled":true,"uapsdEnabled":false}
  ],
  "firewallPolicies": [
    {"name":"iot-to-internal-block","enabled":true,"action":"BLOCK",
     "allowReturnTraffic":true,"sourceZone":"iot","destinationZone":"internal",
     "ipVersion":"IPV4_AND_IPV6","loggingEnabled":false,"order":10},
    {"name":"internal-to-iot-mgmt","enabled":true,"action":"ALLOW",
     "allowReturnTraffic":true,"sourceZone":"internal","destinationZone":"iot",
     "protocol":"TCP","destination":{"type":"PORT","portFilter":{"items":["22","8000-8100"],"matchOpposite":false}},
     "connectionStates":["NEW","ESTABLISHED","RELATED"],
     "ipVersion":"IPV4_AND_IPV6","loggingEnabled":false,"order":20},
    {"name":"iot-curfew","enabled":true,"action":"BLOCK",
     "allowReturnTraffic":true,"sourceZone":"iot","destinationZone":"internal",
     "source":{"type":"MAC_ADDRESS","macAddressFilter":{"macAddresses":["02:00:5e:10:00:01","02:00:5e:10:00:02"]}},
     "schedule":{"mode":"CUSTOM","startTime":"21:00","stopTime":"07:00",
       "repeatOnDays":["MONDAY","TUESDAY","WEDNESDAY","THURSDAY","SUNDAY"]},
     "ipVersion":"IPV4_AND_IPV6","loggingEnabled":false,"order":30}
  ],
  "dnsPolicies": [
    {"type":"A_RECORD","enabled":true,"domain":"nas.example.internal","ipv4Address":"192.0.2.10","ttlSeconds":0},
    {"type":"CNAME_RECORD","enabled":true,"domain":"files.example.internal","targetDomain":"nas.example.internal","ttlSeconds":0}
  ]
}`

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(i int) string {
	return string(rune('0'+i/100%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10))
}

// seedFirewall gives the fake a zone-based firewall shaped like a real
// console's: two user zones plus a system zone, and — crucially — several
// SYSTEM_DEFINED policies that share the same name, as a stock console has.
func seedFirewall(f *fakeConsole) (internal, iot, gateway string) {
	defaultNet := f.objectNamed(collNetworks, "Default")["id"].(string)
	iotNet := f.objectNamed(collNetworks, "IoT")["id"].(string)
	internal = f.seed(collZones, originSystem, map[string]any{"name": "internal", "networkIds": []any{defaultNet}})
	iot = f.seed(collZones, originUser, map[string]any{"name": "iot", "networkIds": []any{iotNet}})
	gateway = f.seed(collZones, originSystem, map[string]any{"name": "gateway", "networkIds": []any{}})

	// "Allow All Traffic" three times over, on three different zone pairs.
	for _, pair := range [][2]string{{internal, internal}, {iot, iot}, {internal, gateway}} {
		f.seed(collPolicies, originSystem, map[string]any{
			"name": "Allow All Traffic", "enabled": true,
			"action":          map[string]any{"type": "ALLOW", "allowReturnTraffic": true},
			"source":          map[string]any{"zoneId": pair[0]},
			"destination":     map[string]any{"zoneId": pair[1]},
			"ipProtocolScope": map[string]any{"ipVersion": "IPV4_AND_IPV6"},
			"loggingEnabled":  false,
		})
	}
	return internal, iot, gateway
}

// TestPoliciesAreKeyedByZonePairAndName is the fix for the bug that made the
// firewall unusable: a stock console reuses "Allow All Traffic" across dozens
// of policies, so name-keyed matching mapped every one of them onto the same
// live object and the plan never converged. Declaring all three must be a
// no-op, and changing exactly one must touch exactly that one.
func TestPoliciesAreKeyedByZonePairAndName(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	internal, iot, gateway := seedFirewall(f)

	const declared = `{
	  "networks": [], "wifi": [], "dnsPolicies": [], "firewallZones": [],
	  "firewallPolicies": [
	    {"name":"Allow All Traffic","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
	     "sourceZone":"internal","destinationZone":"internal","ipVersion":"IPV4_AND_IPV6","loggingEnabled":false},
	    {"name":"Allow All Traffic","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"iot","ipVersion":"IPV4_AND_IPV6","loggingEnabled":%s},
	    {"name":"Allow All Traffic","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
	     "sourceZone":"internal","destinationZone":"gateway","ipVersion":"IPV4_AND_IPV6","loggingEnabled":false}
	  ]
	}`

	// All three declared as they are: nothing to do.
	stdout, _, code := run(t, f, fmt.Sprintf(declared, "false"), nil, "diff")
	if code != 0 {
		t.Fatalf("three same-named policies did not converge (exit %d):\n%s", code, stdout)
	}
	if n := strings.Count(stdout, "OK     firewall policy"); n != 3 {
		t.Errorf("want three matched policies, got %d:\n%s", n, stdout)
	}

	// Flip logging on the iot->iot one only.
	stdout = mustRun(t, f, fmt.Sprintf(declared, "true"), nil, "sync")
	if n := strings.Count(stdout, "UPDATE"); n != 1 {
		t.Fatalf("want exactly one update, got:\n%s", stdout)
	}
	if got := f.policyNamed("Allow All Traffic", iot, iot); got["loggingEnabled"] != true {
		t.Errorf("the iot->iot policy was not updated: %v", got)
	}
	for _, pair := range [][2]string{{internal, internal}, {internal, gateway}} {
		if got := f.policyNamed("Allow All Traffic", pair[0], pair[1]); got["loggingEnabled"] != false {
			t.Errorf("a same-named policy on another zone pair was disturbed: %v", got)
		}
	}
}

// TestDuplicatePolicyIdentityIsRejected: since the identity is a triple, two
// entries sharing it are ambiguous. Silently reconciling the last one would
// leave the first permanently unapplied.
func TestDuplicatePolicyIdentityIsRejected(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	seedFirewall(f)

	desired := `{
	  "networks": [], "wifi": [], "dnsPolicies": [], "firewallZones": [],
	  "firewallPolicies": [
	    {"name":"dup","enabled":true,"action":"BLOCK","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4","loggingEnabled":false},
	    {"name":"dup","enabled":false,"action":"ALLOW","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4","loggingEnabled":true}
	  ]
	}`
	_, stderr, code := run(t, f, desired, nil, "sync")
	if code == 0 {
		t.Fatal("two policies with the same identity were accepted")
	}
	if !strings.Contains(stderr, "share the identity") {
		t.Errorf("error does not explain the clash: %s", stderr)
	}
	for _, m := range f.recorded() {
		if strings.HasPrefix(m.Path, collPolicies) {
			t.Errorf("a policy was written despite the ambiguity: %+v", m)
		}
	}
}

// TestAmbiguousLivePolicyBlocksOnlyItself: an operator can hand-make two
// console policies that share the identity triple. Those two cannot be
// reconciled, but every other policy still can — an unrelated clash must not
// take the whole site down.
func TestAmbiguousLivePolicyBlocksOnlyItself(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	_, iot, _ := seedFirewall(f)
	for range 2 {
		f.seed(collPolicies, originUser, map[string]any{
			"name": "hand-made", "enabled": true,
			"action":          map[string]any{"type": "BLOCK"},
			"source":          map[string]any{"zoneId": iot},
			"destination":     map[string]any{"zoneId": iot},
			"ipProtocolScope": map[string]any{"ipVersion": "IPV4"},
			"loggingEnabled":  false,
		})
	}

	// Declaring an unrelated policy is unaffected.
	unrelated := `{
	  "networks": [], "wifi": [], "dnsPolicies": [], "firewallZones": [],
	  "firewallPolicies": [
	    {"name":"Allow All Traffic","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"iot","ipVersion":"IPV4_AND_IPV6","loggingEnabled":false}
	  ]
	}`
	if stdout, stderr, code := run(t, f, unrelated, nil, "diff"); code != 0 {
		t.Errorf("an unrelated clash blocked the whole plan (exit %d)\n%s\n%s", code, stdout, stderr)
	}

	// Declaring the ambiguous one is refused.
	desired := `{
	  "networks": [], "wifi": [], "dnsPolicies": [], "firewallZones": [],
	  "firewallPolicies": [
	    {"name":"hand-made","enabled":true,"action":"BLOCK","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"iot","ipVersion":"IPV4","loggingEnabled":false}
	  ]
	}`
	if _, stderr, code := run(t, f, desired, nil, "sync"); code == 0 || !strings.Contains(stderr, "cannot tell them apart") {
		t.Errorf("reconciling an ambiguous policy exited %d: %s", code, stderr)
	}

	// And so is pruning it: --prune must not pick one of the two to delete.
	if _, stderr, code := run(t, f, unrelated, nil, "sync", "--prune"); code == 0 || !strings.Contains(stderr, "cannot tell them apart") {
		t.Errorf("prune with an ambiguous policy exited %d: %s", code, stderr)
	}
}

// TestUnmodelledPolicyFieldsAreRefused is the safety rule that made issue #11
// urgent: `diff` compares only modelled fields, so a policy carrying a filter
// the schema does not know about looked like a clean no-op while `sync` would
// PUT it back stripped — turning "block one app for fifteen devices in the
// evening" into "block the whole LAN". Neither command may plan that write.
func TestUnmodelledPolicyFieldsAreRefused(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	_, iot, _ := seedFirewall(f)

	// A live policy with a filter cmd/unifi cannot express.
	f.seed(collPolicies, originUser, map[string]any{
		"name": "block-the-app", "enabled": true,
		"action": map[string]any{"type": "BLOCK"},
		"source": map[string]any{"zoneId": iot},
		"destination": map[string]any{"zoneId": iot, "trafficFilter": map[string]any{
			"type":            "WEB_DOMAIN",
			"webDomainFilter": map[string]any{"domains": []any{"example.invalid"}},
		}},
		"ipProtocolScope": map[string]any{"ipVersion": "IPV4_AND_IPV6"},
		"loggingEnabled":  false,
	})

	desired := `{
	  "networks": [], "wifi": [], "dnsPolicies": [], "firewallZones": [],
	  "firewallPolicies": [
	    {"name":"block-the-app","enabled":true,"action":"BLOCK","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"iot","ipVersion":"IPV4_AND_IPV6","loggingEnabled":false}
	  ]
	}`

	for _, cmd := range []string{"diff", "sync"} {
		t.Run(cmd, func(t *testing.T) {
			_, stderr, code := run(t, f, desired, nil, cmd)
			if code != 1 {
				t.Fatalf("%s exited %d; an unmodelled field must be a hard failure, not drift or a no-op", cmd, code)
			}
			if !strings.Contains(stderr, "does not model") || !strings.Contains(stderr, "webDomainFilter") {
				t.Errorf("error does not name the field that would be lost: %s", stderr)
			}
		})
	}
	for _, m := range f.recorded() {
		if strings.HasPrefix(m.Path, collPolicies) {
			t.Errorf("a lossy write was issued: %+v", m)
		}
	}

	// And `export` leaves it out rather than emitting a version that would
	// become that same lossy PUT.
	stdout, stderr, code := run(t, f, "", nil, "export")
	if code != 0 {
		t.Fatalf("export failed: %s", stderr)
	}
	if strings.Contains(stdout, "block-the-app") {
		t.Errorf("export emitted a policy it cannot round-trip:\n%s", stdout)
	}
	if !strings.Contains(stderr, "block-the-app") {
		t.Errorf("export skipped a policy without saying so: %s", stderr)
	}
}

// TestExportRoundTripsEveryPolicy is the acceptance criterion from issue #11:
// a console's full policy set — every filter kind, a schedule, duplicate names
// — must export and diff back as a no-op.
func TestExportRoundTripsEveryPolicy(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	internal, iot, gateway := seedFirewall(f)

	f.seed(collPolicies, originSystem, map[string]any{
		"name": "Allow mDNS", "enabled": true,
		"action": map[string]any{"type": "ALLOW", "allowReturnTraffic": true},
		"source": map[string]any{"zoneId": internal, "trafficFilter": map[string]any{
			"type": "PORT",
			"portFilter": map[string]any{"type": "PORTS", "matchOpposite": false,
				"items": []any{map[string]any{"type": "PORT_NUMBER", "value": 5353}}},
		}},
		"destination": map[string]any{"zoneId": gateway, "trafficFilter": map[string]any{
			"type": "IP_ADDRESS",
			"ipAddressFilter": map[string]any{"type": "IP_ADDRESSES", "matchOpposite": false,
				"items": []any{
					map[string]any{"type": "IP_ADDRESS", "value": "224.0.0.251"},
					map[string]any{"type": "SUBNET", "value": "fe80::/10"},
				}},
			"portFilter": map[string]any{"type": "PORTS", "matchOpposite": false,
				"items": []any{map[string]any{"type": "PORT_NUMBER", "value": 5353}}},
		}},
		"ipProtocolScope": map[string]any{"ipVersion": "IPV4_AND_IPV6",
			"protocolFilter": map[string]any{"type": "NAMED_PROTOCOL", "matchOpposite": false,
				"protocol": map[string]any{"name": "UDP"}}},
		"connectionStateFilter": []any{"RELATED", "ESTABLISHED"},
		"loggingEnabled":        true,
	})
	f.seed(collPolicies, originUser, map[string]any{
		"name": "curfew", "enabled": true,
		"action": map[string]any{"type": "BLOCK"},
		"source": map[string]any{"zoneId": iot, "trafficFilter": map[string]any{
			"type":             "MAC_ADDRESS",
			"macAddressFilter": map[string]any{"macAddresses": []any{"02:00:5e:10:00:01", "02:00:5e:10:00:02"}},
		}},
		"destination": map[string]any{"zoneId": gateway, "trafficFilter": map[string]any{
			"type":              "APPLICATION",
			"applicationFilter": map[string]any{"applicationIds": []any{262392, 262256}},
		}},
		"ipProtocolScope": map[string]any{"ipVersion": "IPV4_AND_IPV6"},
		"loggingEnabled":  false,
		"schedule": map[string]any{"mode": "CUSTOM",
			"timeFilter":   map[string]any{"startTime": "23:00", "stopTime": "07:00"},
			"repeatOnDays": []any{"MONDAY", "SUNDAY"},
			"startDate":    "2026-03-14", "stopDate": "2026-03-21"},
	})

	exported, stderr, code := run(t, f, "", nil, "export")
	if code != 0 {
		t.Fatalf("export failed: %s", stderr)
	}
	if stderr != "" {
		t.Fatalf("export could not represent everything: %s", stderr)
	}
	var doc site
	if err := json.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.FirewallPolicies) != 5 {
		t.Fatalf("export returned %d policies, want 5:\n%s", len(doc.FirewallPolicies), exported)
	}
	// The MAC/application/schedule policy must survive with its detail intact.
	var curfew *firewallPolicy
	for i := range doc.FirewallPolicies {
		if doc.FirewallPolicies[i].Name == "curfew" {
			curfew = &doc.FirewallPolicies[i]
		}
	}
	if curfew == nil {
		t.Fatalf("export lost the scheduled policy:\n%s", exported)
	}
	if curfew.Source == nil || curfew.Source.MACAddressFilter == nil ||
		len(curfew.Source.MACAddressFilter.MACAddresses) != 2 {
		t.Errorf("export dropped the MAC filter: %+v", curfew.Source)
	}
	if curfew.Schedule == nil || curfew.Schedule.StopDate != "2026-03-21" {
		t.Errorf("export dropped the schedule: %+v", curfew.Schedule)
	}
	if curfew.Order == nil {
		t.Error("export gave a USER_DEFINED policy no order")
	}

	env := []string{"UNIFI_WIFI_EXAMPLE_MAIN=super-secret-passphrase"}
	stdout, _, code := run(t, f, exported, env, "diff")
	if code != 0 {
		t.Fatalf("diff of exported state wanted exit 0, got %d:\n%s", code, stdout)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line != "" && !strings.HasPrefix(line, "OK") && !strings.HasPrefix(line, "DRY RUN") {
			t.Errorf("export→diff round trip is not a no-op: %q", line)
		}
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Errorf("export and diff mutated the console: %+v", muts)
	}
}

// TestIdlessPolicyCannotBeWritten pins the firmware limitation from
// docs/unifi-api-notes.md: UniFi Network 10.6 returns USER_DEFINED policies
// without an id, so there is no URL to PUT or DELETE against. The tool has to
// say so rather than guess.
func TestIdlessPolicyCannotBeWritten(t *testing.T) {
	f := newFakeConsole(t)
	f.omitUserPolicyIDs = true
	seedSite(f)
	_, iot, _ := seedFirewall(f)
	f.seed(collPolicies, originUser, map[string]any{
		"name": "night-owls", "enabled": true,
		"action":          map[string]any{"type": "BLOCK"},
		"source":          map[string]any{"zoneId": iot},
		"destination":     map[string]any{"zoneId": iot},
		"ipProtocolScope": map[string]any{"ipVersion": "IPV4"},
		"loggingEnabled":  false,
	})

	const desired = `{
	  "networks": [], "wifi": [], "dnsPolicies": [], "firewallZones": [],
	  "firewallPolicies": [
	    {"name":"night-owls","enabled":%s,"action":"BLOCK","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"iot","ipVersion":"IPV4","loggingEnabled":false}
	  ]
	}`

	// Unchanged: an id-less policy still reads cleanly.
	if stdout, stderr, code := run(t, f, fmt.Sprintf(desired, "true"), nil, "diff"); code != 0 {
		t.Fatalf("an unchanged id-less policy must still diff clean (exit %d)\n%s\n%s", code, stdout, stderr)
	}

	// Changed: the update is impossible and must be reported, not skipped.
	_, stderr, code := run(t, f, fmt.Sprintf(desired, "false"), nil, "sync")
	if code == 0 {
		t.Fatal("an update to an id-less policy was silently accepted")
	}
	if !strings.Contains(stderr, "without an id") {
		t.Errorf("error does not explain the missing id: %s", stderr)
	}

	// So is deleting one.
	empty := `{"networks":[],"wifi":[],"dnsPolicies":[],"firewallZones":[],
	  "firewallPolicies":[{"name":"other","enabled":true,"action":"BLOCK","allowReturnTraffic":true,
	    "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4","loggingEnabled":false}]}`
	if _, stderr, code := run(t, f, empty, nil, "sync", "--prune"); code == 0 || !strings.Contains(stderr, "without an id") {
		t.Errorf("deleting an id-less policy exited %d: %s", code, stderr)
	}
}

// TestZoneWithUnnameableMembersIsRefused covers the console's `External` zone,
// whose members are WAN interfaces that GET /networks does not return. Writing
// it back would empty it, so export leaves it out and sync refuses to change it.
func TestZoneWithUnnameableMembersIsRefused(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	f.seed(collZones, originSystem, map[string]any{
		"name": "External", "networkIds": []any{"wan-1", "wan-2"},
	})

	stdout, stderr, code := run(t, f, "", nil, "export")
	if code != 0 {
		t.Fatalf("export failed: %s", stderr)
	}
	var doc site
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	for _, z := range doc.FirewallZones {
		if z.Name == "External" {
			t.Errorf(`export emitted External as %v; a zone whose members cannot be named must be skipped`, z.Networks)
		}
	}
	if !strings.Contains(stderr, "External") {
		t.Errorf("export skipped a zone without saying so: %s", stderr)
	}

	desired := `{"networks":[],"wifi":[],"dnsPolicies":[],"firewallPolicies":[],
	  "firewallZones":[{"name":"External","networks":[]}]}`
	_, stderr, code = run(t, f, desired, nil, "sync")
	if code == 0 {
		t.Fatal("declaring External with no members was accepted; that PUT would empty the zone")
	}
	if !strings.Contains(stderr, "WAN") {
		t.Errorf("error does not explain why: %s", stderr)
	}
}

// TestFirewallPolicyOrdering exercises ordering end to end. The console orders
// policies per zone pair, so the write has to carry the pair and must only
// move that pair's policies.
func TestFirewallPolicyOrdering(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)

	desired := `{
	  "networks": [], "wifi": [], "dnsPolicies": [],
	  "firewallZones": [
	    {"name":"internal","networks":["Default"]},
	    {"name":"iot","networks":["IoT"]}
	  ],
	  "firewallPolicies": [
	    {"name":"alpha-runs-last","enabled":true,"action":"BLOCK","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4",
	     "loggingEnabled":false,"order":30},
	    {"name":"zulu-runs-first","enabled":true,"action":"BLOCK","allowReturnTraffic":true,
	     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4",
	     "protocol":"TCP","destination":{"type":"PORT","portFilter":{"items":["443","8000-8100"],"matchOpposite":false}},
	     "loggingEnabled":false,"order":10},
	    {"name":"mike-runs-second","enabled":true,"action":"REJECT","allowReturnTraffic":false,
	     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4",
	     "loggingEnabled":true,"order":20}
	  ]
	}`

	mustRun(t, f, desired, nil, "sync")

	if want := []string{"zulu-runs-first", "mike-runs-second", "alpha-runs-last"}; !equalStrings(f.policyOrder(), want) {
		t.Errorf("policies ordered %v, want %v", f.policyOrder(), want)
	}
	// The ordering call must name the zone pair; without the query parameters
	// a live console answers 400.
	var orderings int
	for _, m := range f.recorded() {
		if m.Path == collPolicies+"/ordering" {
			orderings++
		}
	}
	if orderings != 1 {
		t.Errorf("want one ordering call, got %d", orderings)
	}

	// "443" and "8000-8100" must reach the API as the two different item
	// shapes it distinguishes, both under `value`.
	iotZone := f.objectNamed(collZones, "iot")["id"].(string)
	internalZone := f.objectNamed(collZones, "internal")["id"].(string)
	policy := f.policyNamed("zulu-runs-first", iotZone, internalZone)
	dst, _ := policy["destination"].(map[string]any)
	tf, _ := dst["trafficFilter"].(map[string]any)
	pf, _ := tf["portFilter"].(map[string]any)
	items, _ := pf["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("destination ports rendered as %v", pf)
	}
	single, _ := items[0].(map[string]any)
	if single["type"] != "PORT_NUMBER" || single["value"] != float64(443) {
		t.Errorf("port 443 rendered as %v", single)
	}
	ranged, _ := items[1].(map[string]any)
	if ranged["type"] != "PORT_NUMBER_RANGE" || ranged["value"] != "8000-8100" {
		t.Errorf("port range 8000-8100 rendered as %v", ranged)
	}

	before := len(f.recorded())
	if out := mustRun(t, f, desired, nil, "sync"); strings.Contains(out, "ORDER") {
		t.Errorf("second sync reordered again:\n%s", out)
	}
	if after := len(f.recorded()); after != before {
		t.Errorf("second sync made %d further writes; want none", after-before)
	}
}

// TestInvalidPortIsRejected: the CUE regex cannot express the 65535 upper
// bound on a string, so the tool is the last line of defence. A port it cannot
// parse must fail the sync — sending 0 would quietly open or close the wrong
// traffic.
func TestInvalidPortIsRejected(t *testing.T) {
	for _, port := range []string{"70000", "0", "8100-8000", "abc"} {
		t.Run(port, func(t *testing.T) {
			f := newFakeConsole(t)
			seedSite(f)
			desired := `{
			  "networks": [], "wifi": [], "dnsPolicies": [],
			  "firewallZones": [{"name":"internal","networks":["Default"]}],
			  "firewallPolicies": [
			    {"name":"p","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
			     "sourceZone":"internal","destinationZone":"internal","ipVersion":"IPV4",
			     "loggingEnabled":false,"order":10,
			     "destination":{"type":"PORT","portFilter":{"items":["` + port + `"],"matchOpposite":false}}}
			  ]
			}`
			stdout, stderr, code := run(t, f, desired, nil, "sync")
			if code == 0 {
				t.Fatalf("port %q was accepted:\n%s", port, stdout)
			}
			if !strings.Contains(stderr, "port") {
				t.Errorf("error does not mention the port: %s", stderr)
			}
			for _, m := range f.recorded() {
				if strings.HasPrefix(m.Path, collPolicies) {
					t.Errorf("a policy was written despite the invalid port: %+v", m)
				}
			}
		})
	}
}

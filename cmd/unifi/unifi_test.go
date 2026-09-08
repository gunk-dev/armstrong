package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
     "ipVersion":"IPV4_AND_IPV6","loggingEnabled":false,"order":10}
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

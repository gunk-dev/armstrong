package main

import (
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// presetPolicies declares a TCP_UDP policy (a PRESET on the console) beside a
// UDP one (a NAMED_PROTOCOL), and the console's own TCP_UDP "Allow DNS" as it
// stands.
const presetPolicies = `{
  "firewallPolicies": [
    {"name":"Allow DNS","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
     "sourceZone":"iot","destinationZone":"gateway","ipVersion":"IPV4_AND_IPV6",
     "protocol":"TCP_UDP","destination":{"type":"PORT","portFilter":{"items":["53"]}}},
    {"name":"casting and printing","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4_AND_IPV6","protocol":"TCP_UDP"},
    {"name":"mDNS","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4_AND_IPV6","protocol":"UDP"}
  ]
}`

// seedPresetDNS seeds a SYSTEM_DEFINED policy in the shape a live 10.6
// console reports its "Allow DNS": a PRESET filter carrying no matchOpposite.
func seedPresetDNS(f *fakeConsole, src, dst string) {
	f.seed(collPolicies, originSystem, map[string]any{
		"name": "Allow DNS", "enabled": true,
		"action": map[string]any{"type": "ALLOW", "allowReturnTraffic": true},
		"source": map[string]any{"zoneId": src},
		"destination": map[string]any{"zoneId": dst, "trafficFilter": map[string]any{
			"type": "PORT",
			"portFilter": map[string]any{"type": "PORTS", "matchOpposite": false,
				"items": []any{map[string]any{"type": "PORT_NUMBER", "value": 53}}},
		}},
		"ipProtocolScope": map[string]any{"ipVersion": "IPV4_AND_IPV6",
			"protocolFilter": map[string]any{"type": "PRESET", "preset": map[string]any{"name": "TCP_UDP"}}},
		"loggingEnabled": false,
	})
}

// TestSyncTCPUDPPolicyConverges: a TCP_UDP policy has to be sent as the PRESET
// the console accepts, and read back as TCP_UDP so that the next run plans
// nothing. The console's own PRESET policy must project the same way.
func TestSyncTCPUDPPolicyConverges(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	internal, iot, gateway := seedFirewall(f)
	seedPresetDNS(f, iot, gateway)

	out := mustRun(t, f, presetPolicies, nil, "sync")
	if !strings.Contains(out, "OK     firewall policy iot -> gateway / Allow DNS") {
		t.Errorf("the live PRESET policy did not project as TCP_UDP:\n%s", out)
	}

	byName := map[string]map[string]any{}
	for _, m := range f.recorded() {
		if m.Method != "POST" || m.Path != collPolicies {
			t.Errorf("unexpected write %s %s", m.Method, m.Path)
			continue
		}
		byName[m.Body["name"].(string)] = m.Body["ipProtocolScope"].(map[string]any)["protocolFilter"].(map[string]any)
	}
	wantFilters := map[string]map[string]any{
		"casting and printing": {"type": "PRESET", "preset": map[string]any{"name": "TCP_UDP"}},
		"mDNS":                 {"type": "NAMED_PROTOCOL", "protocol": map[string]any{"name": "UDP"}, "matchOpposite": false},
	}
	if !reflect.DeepEqual(byName, wantFilters) {
		t.Errorf("protocol filters sent:\n got %v\nwant %v", byName, wantFilters)
	}
	if f.policyNamed("casting and printing", iot, internal) == nil {
		t.Fatal("the TCP_UDP policy was not created")
	}

	out = mustRun(t, f, presetPolicies, nil, "diff")
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "OK ") && !strings.HasPrefix(line, "DRY RUN") {
			t.Errorf("second run planned work: %s", line)
		}
	}

	// export models the PRESET policies rather than skipping them.
	exported, stderr, code := run(t, f, "", nil, "export")
	if code != 0 || stderr != "" {
		t.Fatalf("export exited %d: %s", code, stderr)
	}
	var doc site
	if err := json.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatal(err)
	}
	protocols := map[string]string{}
	for _, p := range doc.FirewallPolicies {
		protocols[p.Name] = p.Protocol
	}
	for name, want := range map[string]string{"Allow DNS": "TCP_UDP", "casting and printing": "TCP_UDP", "mDNS": "UDP"} {
		if protocols[name] != want {
			t.Errorf("export: %q has protocol %q, want %q", name, protocols[name], want)
		}
	}
	mustRun(t, f, exported, mainPassphrase, "diff")
}

// TestNegatedTCPUDPRefusedBeforeAnyWrite: the console refuses matchOpposite on
// a PRESET, so a negated TCP_UDP fails the run before anything is written —
// including the network the same instance file creates first.
func TestNegatedTCPUDPRefusedBeforeAnyWrite(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	seedFirewall(f)

	const declared = `{
	  "firewallPolicies": [
	    {"name":"other protocol block","enabled":true,"action":"BLOCK",
	     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4_AND_IPV6",
	     "protocol":"TCP_UDP","protocolMatchOpposite":true}
	  ]
	}`
	_, stderr, code := run(t, f, declared, nil, "sync")
	if code == 0 {
		t.Fatal("sync accepted a negated TCP_UDP")
	}
	if !strings.Contains(stderr, "cannot be negated") {
		t.Errorf("error does not say why:\n%s", stderr)
	}
	if m := f.recorded(); len(m) != 0 {
		t.Errorf("sync wrote before refusing: %v", m)
	}
}

// TestFakeEnforcesProtocolFilterRules pins the fake to what a live console
// answered, so the tests above cannot pass against a fake that accepts
// anything.
func TestFakeEnforcesProtocolFilterRules(t *testing.T) {
	for _, tc := range []struct {
		filter map[string]any
		code   string
	}{
		{map[string]any{"type": "NAMED_PROTOCOL", "protocol": map[string]any{"name": "TCP_UDP"}, "matchOpposite": false}, "api.request.unknown-type-id"},
		{map[string]any{"type": "PRESET", "preset": map[string]any{"name": "TCP_UDP"}, "matchOpposite": true}, "api.request.unknown-property"},
		{map[string]any{"type": "PRESET", "preset": map[string]any{"name": "TCP_UDP"}}, ""},
		{map[string]any{"type": "NAMED_PROTOCOL", "protocol": map[string]any{"name": "ICMP"}, "matchOpposite": true}, ""},
	} {
		code, _ := protocolFilterFault(map[string]any{"ipProtocolScope": map[string]any{"protocolFilter": tc.filter}})
		if code != tc.code {
			t.Errorf("%v: got %q, want %q", tc.filter, code, tc.code)
		}
	}
}

func TestCueVetRejectsNegatedPreset(t *testing.T) {
	if _, err := exec.LookPath("cue"); err != nil {
		t.Skip("cue not installed")
	}
	cmd := exec.Command("cue", "vet", "-c", "./cmd/unifi/testdata/protocol-invalid/negated-tcp-udp")
	cmd.Dir = "../.."
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("cue vet accepted a negated TCP_UDP")
	}
	if !strings.Contains(string(out), "_TCP_UDP_is_a_PRESET_filter_which_the_console_refuses_to_negate") {
		t.Errorf("cue vet rejected it for something else:\n%s", out)
	}
}

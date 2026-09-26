package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// lintCases holds one #Site per console validation rule, each breaking only
// that rule. cueDir names the testdata instance `cue vet -c` must reject, and
// cueField the hidden field it must name, where schema/unifi.cue encodes the
// rule.
var lintCases = []struct {
	rule, site, cueDir, cueField string
}{
	{
		rule: "network-in-one-zone",
		site: `{"networks":[{"name":"Guest","vlanId":20}],
		  "firewallZones":[{"name":"a","networks":["Guest"]},{"name":"b","networks":["Guest"]}]}`,
	},
	{
		rule: "network-needs-zone",
		site: `{"networks":[{"name":"Guest","vlanId":20}],"firewallZones":[{"name":"a","networks":[]}]}`,
	},
	{
		rule: "tcp-udp-not-negated",
		site: `{"firewallPolicies":[{"name":"p","action":"BLOCK","sourceZone":"iot","destinationZone":"internal",
		  "ipVersion":"IPV4_AND_IPV6","protocol":"TCP_UDP","protocolMatchOpposite":true}]}`,
		cueDir: "protocol-invalid/negated-tcp-udp", cueField: "_TCP_UDP_is_a_PRESET_filter_which_the_console_refuses_to_negate",
	},
	{
		rule: "icmp-ip-version",
		site: `{"firewallPolicies":[{"name":"p","action":"BLOCK","sourceZone":"iot","destinationZone":"internal",
		  "ipVersion":"IPV4_AND_IPV6","protocol":"ICMP"}]}`,
		cueDir: "lint-invalid/icmp-ip-version", cueField: "_ICMP_is_accepted_only_with_ipVersion_IPV4",
	},
	{
		rule: "return-traffic-states",
		site: `{"firewallPolicies":[{"name":"replies","action":"ALLOW","allowReturnTraffic":true,
		  "sourceZone":"media","destinationZone":"guest","ipVersion":"IPV4_AND_IPV6",
		  "connectionStates":["RELATED","ESTABLISHED"]}]}`,
		cueDir: "lint-invalid/return-traffic-states", cueField: "_return_traffic_cannot_be_allowed_when_connectionStates_are_only_ESTABLISHED_and_RELATED",
	},
	{
		rule: "return-traffic-zone",
		site: `{"firewallPolicies":[{"name":"guest dns","action":"ALLOW","allowReturnTraffic":true,
		  "sourceZone":"guest","destinationZone":"Gateway","ipVersion":"IPV4_AND_IPV6","protocol":"UDP"}]}`,
		cueDir: "lint-invalid/return-traffic-zone", cueField: "_return_traffic_cannot_be_allowed_to_or_from_the_Gateway_or_External_zone",
	},
	{
		rule: "ip-address-version",
		site: `{"firewallPolicies":[{"name":"dns","action":"ALLOW","sourceZone":"iot","destinationZone":"internal",
		  "ipVersion":"IPV4","destination":{"type":"IP_ADDRESS",
		  "ipAddressFilter":{"items":[{"type":"IP_ADDRESS","value":"fd00::53"}],"matchOpposite":false}}}]}`,
		cueDir: "lint-invalid/ip-address-version", cueField: "_IP_address_filter_values_must_be_IPv4_addresses_for_ipVersion_IPV4",
	},
}

// TestLint: each rule fails `unifi lint` naming the rule, fails diff and sync
// before their first request to the console, and fails `cue vet` where the
// schema encodes it.
func TestLint(t *testing.T) {
	_, cueErr := exec.LookPath("cue")
	for _, tc := range lintCases {
		t.Run(tc.rule, func(t *testing.T) {
			f := newFakeConsole(t)
			out, _, code := run(t, f, tc.site, nil, "lint")
			if code != 1 || !strings.Contains(out, ": "+tc.rule+": ") {
				t.Errorf("lint exited %d, want 1 naming %s:\n%s", code, tc.rule, out)
			}
			for _, args := range [][]string{{"diff"}, {"sync"}} {
				_, stderr, code := run(t, f, tc.site, nil, args...)
				if code != 1 || !strings.Contains(stderr, ": "+tc.rule+": ") {
					t.Errorf("%v exited %d, want 1 naming %s:\n%s", args, code, tc.rule, stderr)
				}
			}
			if f.requests != 0 {
				t.Errorf("diff/sync made %d requests before refusing", f.requests)
			}

			if tc.cueDir == "" {
				return
			}
			if cueErr != nil {
				t.Skip("cue not installed")
			}
			cmd := exec.Command("cue", "vet", "-c", "./cmd/unifi/testdata/"+tc.cueDir)
			cmd.Dir = "../.."
			vet, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(vet), tc.cueField) {
				t.Errorf("cue vet (err %v) did not name %s:\n%s", err, tc.cueField, vet)
			}
		})
	}
}

// TestLintAcceptsWhatTheConsoleAccepts: shapes a live console took, the
// console's own policies as `unifi export` renders them, and a violating
// policy set back within the rule.
func TestLintAcceptsWhatTheConsoleAccepts(t *testing.T) {
	const accepted = `{"firewallPolicies":[
	  {"name":"a","action":"ALLOW","allowReturnTraffic":true,"sourceZone":"iot","destinationZone":"internal",
	   "ipVersion":"IPV4_AND_IPV6","connectionStates":["ESTABLISHED"]},
	  {"name":"b","action":"ALLOW","allowReturnTraffic":true,"sourceZone":"iot","destinationZone":"internal",
	   "ipVersion":"IPV4_AND_IPV6","connectionStates":["ESTABLISHED","RELATED","NEW"]},
	  {"name":"c","action":"ALLOW","allowReturnTraffic":false,"sourceZone":"media","destinationZone":"guest",
	   "ipVersion":"IPV4_AND_IPV6","connectionStates":["ESTABLISHED","RELATED"]},
	  {"name":"d","action":"ALLOW","allowReturnTraffic":false,"sourceZone":"guest","destinationZone":"Gateway","ipVersion":"IPV4"},
	  {"name":"e","action":"BLOCK","sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV6","protocol":"ICMPV6"},
	  {"name":"f","action":"ALLOW","sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4_AND_IPV6",
	   "destination":{"type":"IP_ADDRESS","ipAddressFilter":{"items":[{"type":"IP_ADDRESS","value":"192.0.2.53"}],"matchOpposite":false}}},
	  {"name":"Allow Return Traffic","action":"ALLOW","allowReturnTraffic":true,"sourceZone":"External","destinationZone":"internal",
	   "ipVersion":"IPV4_AND_IPV6","connectionStates":["RELATED","ESTABLISHED"]},
	  {"name":"casting (Return)","action":"ALLOW","allowReturnTraffic":true,"sourceZone":"media","destinationZone":"guest",
	   "ipVersion":"IPV4_AND_IPV6","connectionStates":["RELATED","ESTABLISHED"]}
	]}`
	f := newFakeConsole(t)
	if out, stderr, code := run(t, f, accepted, nil, "lint"); code != 0 {
		t.Errorf("lint exited %d:\n%s%s", code, out, stderr)
	}
}

// TestFakeEnforcesPolicyCreateRules pins the fake to what a live console
// answered to each rule's policy, rendered as cmd/unifi renders it, so the
// tests above cannot pass against a fake that accepts anything.
func TestFakeEnforcesPolicyCreateRules(t *testing.T) {
	f := newFakeConsole(t)
	zones := map[string]string{}
	for _, name := range []string{"iot", "internal", "media", "guest", "Gateway"} {
		zones[name] = f.seed(collZones, originUser, map[string]any{"name": name, "networkIds": []any{}})
	}
	resolve := func(name string) (string, error) { return zones[name], nil }
	post := func(body map[string]any) (int, string) {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, f.URL+"/proxy/network/integration/v1/sites/"+f.siteID+"/"+collPolicies, bytes.NewReader(raw))
		req.Header.Set("X-API-KEY", f.apiKey)
		resp, err := f.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var e struct{ Code string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return resp.StatusCode, e.Code
	}

	codes := map[string]string{
		"icmp-ip-version":       codeUnknownTypeID,
		"return-traffic-states": codeCantAllowReturnTraffic,
		"return-traffic-zone":   codeCantAllowReturnTraffic,
		"ip-address-version":    codeInvalidIPAddresses,
	}
	for _, tc := range lintCases {
		want, ok := codes[tc.rule]
		if !ok {
			continue
		}
		var doc site
		if err := json.Unmarshal([]byte(tc.site), &doc); err != nil {
			t.Fatal(err)
		}
		body, err := doc.FirewallPolicies[0].body(resolve, resolve)
		if err != nil {
			t.Fatal(err)
		}
		if status, code := post(body); status != http.StatusBadRequest || code != want {
			t.Errorf("%s: fake answered %d %q, want 400 %q", tc.rule, status, code, want)
		}
	}

	block := map[string]any{"name": "b", "enabled": false,
		"action": map[string]any{"type": "BLOCK", "allowReturnTraffic": true},
		"source": map[string]any{"zoneId": zones["iot"]}, "destination": map[string]any{"zoneId": zones["internal"]},
		"ipProtocolScope": map[string]any{"ipVersion": "IPV4_AND_IPV6"}}
	if status, code := post(block); status != http.StatusBadRequest || code != codeUnknownProperty {
		t.Errorf("BLOCK with allowReturnTraffic: fake answered %d %q", status, code)
	}
}

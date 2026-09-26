package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// setPolicies declares every set-valued list a policy has, each with more than
// one item and in an order the fake does not keep.
const setPolicies = `{
  "firewallPolicies": [
    {"name":"casting and printing","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
     "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4_AND_IPV6","protocol":"TCP_UDP",
     "connectionStates":["NEW","ESTABLISHED","RELATED"],
     "destination":{"type":"PORT","portFilter":{"items":["53","853"],"matchOpposite":true}}},
    {"name":"household DNS","enabled":true,"action":"ALLOW","allowReturnTraffic":true,
     "sourceZone":"internal","destinationZone":"gateway","ipVersion":"IPV4_AND_IPV6","protocol":"TCP_UDP",
     "source":{"type":"NETWORK","networkFilter":{"networks":["Default","IoT"],"matchOpposite":false}},
     "destination":{"type":"IP_ADDRESS",
       "ipAddressFilter":{"items":[{"type":"IP_ADDRESS","value":"192.168.1.4"},{"type":"IP_ADDRESS","value":"192.168.1.5"}],"matchOpposite":false},
       "portFilter":{"items":["53","853","8000-8100"],"matchOpposite":false}}},
    {"name":"curfew","enabled":true,"action":"BLOCK",
     "sourceZone":"iot","destinationZone":"gateway","ipVersion":"IPV4_AND_IPV6",
     "source":{"type":"MAC_ADDRESS","macAddressFilter":{"macAddresses":["02:00:5e:10:00:01","02:00:5e:10:00:02"]}},
     "destination":{"type":"APPLICATION","applicationFilter":{"applicationIds":[262256,262392]}},
     "schedule":{"mode":"CUSTOM","startTime":"23:00","stopTime":"07:00","repeatOnDays":["MONDAY","WEDNESDAY","SUNDAY"]}}
  ]
}`

// TestPolicySetsConverge: the console does not keep the order of a policy's
// set-valued lists, so a policy synced once must read back as OK even though
// every list comes back reordered — otherwise it is a perpetual UPDATE that
// counts against --max-changes on every run.
func TestPolicySetsConverge(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	internal, _, gateway := seedFirewall(f)

	mustRun(t, f, setPolicies, nil, "sync")

	// The fake really did store the lists out of order.
	stored := f.policyNamed("household DNS", internal, gateway)
	if stored == nil {
		t.Fatal("the policy was not created")
	}
	ports := stored["destination"].(map[string]any)["trafficFilter"].(map[string]any)["portFilter"].(map[string]any)["items"].([]any)
	if first := ports[0].(map[string]any)["value"]; first != "8000-8100" {
		t.Fatalf("the fake kept the port order it was sent (first item %v)", first)
	}

	stdout, stderr, code := run(t, f, setPolicies, nil, "diff")
	if code != 0 {
		t.Fatalf("second diff exited %d, want a clean no-op\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	assertAllOK(t, stdout)

	// A duplicate item means nothing in a set: declaring one is still a no-op.
	dup := strings.Replace(setPolicies, `"items":["53","853"]`, `"items":["853","53","853"]`, 1)
	stdout, _, code = run(t, f, dup, nil, "diff")
	if code != 0 {
		t.Fatalf("a duplicate port planned work:\n%s", stdout)
	}

	// An empty list is not sent, so it reads back as absent: still a no-op.
	empty := strings.Replace(setPolicies, `"sourceZone":"iot","destinationZone":"gateway",`,
		`"sourceZone":"iot","destinationZone":"gateway","connectionStates":[],`, 1)
	stdout, _, code = run(t, f, empty, nil, "diff")
	if code != 0 {
		t.Fatalf("an empty connectionStates planned work:\n%s", stdout)
	}

	// A real change to a set is still one.
	changed := strings.Replace(setPolicies, `"items":["53","853"]`, `"items":["53","443"]`, 1)
	stdout, _, code = run(t, f, changed, nil, "diff")
	if code != 2 || !strings.Contains(stdout, "UPDATE firewall policy iot -> internal / casting and printing (destination)") {
		t.Fatalf("a changed port list did not plan an update (exit %d):\n%s", code, stdout)
	}
}

// TestExportEmitsSetsInCanonicalOrder: whatever order the console holds a list
// in, export writes it in one fixed order, so re-exports are deterministic.
func TestExportEmitsSetsInCanonicalOrder(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	seedFirewall(f)
	mustRun(t, f, setPolicies, nil, "sync")

	exported := mustRun(t, f, "", nil, "export")
	var doc site
	if err := json.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatal(err)
	}
	byName := map[string]firewallPolicy{}
	for _, p := range doc.FirewallPolicies {
		byName[p.Name] = p
	}
	for _, c := range []struct {
		what      string
		got, want any
	}{
		{"connection states", byName["casting and printing"].ConnectionStates, []string{"ESTABLISHED", "NEW", "RELATED"}},
		{"ports", byName["household DNS"].Destination.PortFilter.Items, []string{"53", "853", "8000-8100"}},
		{"addresses", byName["household DNS"].Destination.IPAddressFilter.Items,
			[]ipAddressMatch{{"IP_ADDRESS", "192.168.1.4"}, {"IP_ADDRESS", "192.168.1.5"}}},
		{"networks", byName["household DNS"].Source.NetworkFilter.Networks, []string{"Default", "IoT"}},
		{"macs", byName["curfew"].Source.MACAddressFilter.MACAddresses, []string{"02:00:5e:10:00:01", "02:00:5e:10:00:02"}},
		{"applications", byName["curfew"].Destination.ApplicationFilter.ApplicationIDs, []int{262256, 262392}},
		{"days", byName["curfew"].Schedule.RepeatOnDays, []string{"MONDAY", "WEDNESDAY", "SUNDAY"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("export %s: got %v, want %v", c.what, c.got, c.want)
		}
	}

	stdout := mustRun(t, f, exported, mainPassphrase, "diff")
	assertAllOK(t, stdout)
}

func TestCanonicalOrder(t *testing.T) {
	ports := canonicalSet([]string{"8000-8100", "853", "1000", "53", "53"}, comparePorts)
	if want := []string{"53", "853", "1000", "8000-8100"}; !reflect.DeepEqual(ports, want) {
		t.Errorf("ports: got %v, want %v", ports, want)
	}
	ips := canonicalSet([]ipAddressMatch{
		{"SUBNET", "fe80::/10"}, {"SUBNET", "10.0.0.0/8"}, {"SUBNET", "9.0.0.0/8"}, {"IP_ADDRESS", "224.0.0.251"},
	}, compareIPItems)
	want := []ipAddressMatch{{"IP_ADDRESS", "224.0.0.251"}, {"SUBNET", "9.0.0.0/8"}, {"SUBNET", "10.0.0.0/8"}, {"SUBNET", "fe80::/10"}}
	if !reflect.DeepEqual(ips, want) {
		t.Errorf("addresses: got %v, want %v", ips, want)
	}
}

func assertAllOK(t *testing.T, plan string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(plan), "\n") {
		if line != "" && !strings.HasPrefix(line, "OK ") && !strings.HasPrefix(line, "DRY RUN") {
			t.Errorf("planned work: %s", line)
		}
	}
}

package main

import (
	"fmt"
	"strings"
	"testing"
)

// seedZonedConsole is a console running the zone-based firewall, as shipped:
// the Default network in the system Internal zone, and an empty system
// Hotspot zone.
func seedZonedConsole(f *fakeConsole) {
	def := f.seed(collNetworks, originSystem, map[string]any{
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
	internal := f.seed(collZones, originSystem, map[string]any{"name": "Internal", "networkIds": []any{def}})
	f.get(collNetworks, def)["zoneId"] = internal
	f.seed(collZones, originSystem, map[string]any{"name": "Hotspot", "networkIds": []any{}})
}

// zonedNetwork is a #Network entry on the /24 whose first three octets are
// prefix.
func zonedNetwork(name string, vlan int, prefix string) string {
	return fmt.Sprintf(`{"name":%q,"management":"GATEWAY","enabled":true,"vlanId":%d,
	  "isolationEnabled":false,"internetAccessEnabled":true,"cellularBackupEnabled":false,"mdnsForwardingEnabled":false,
	  "ipv4":{"hostIpAddress":"%[3]s.1","prefixLength":24,"autoScaleEnabled":false,
	    "dhcp":{"mode":"SERVER","rangeStart":"%[3]s.100","rangeStop":"%[3]s.199","leaseTimeSeconds":86400}}}`, name, vlan, prefix)
}

// zonedSite mirrors the shape of a first rollout on a zone-based-firewall
// console: new networks each in a new zone, one new network in the existing
// empty Hotspot zone, an SSID on a new network, and a policy between two new
// zones.
func zonedSite() string {
	return `{
	  "networks": [` + strings.Join([]string{
		zonedNetwork("Default", 1, "192.0.2"),
		zonedNetwork("People", 10, "10.0.10"),
		zonedNetwork("IoT", 20, "10.0.20"),
		zonedNetwork("Guest", 50, "10.0.50"),
	}, ",") + `],
	  "firewallZones": [
	    {"name":"Internal","networks":["Default"]},
	    {"name":"Trusted","networks":["People"]},
	    {"name":"Things","networks":["IoT"]},
	    {"name":"Hotspot","networks":["Guest"]}
	  ],
	  "wifi": [
	    {"name":"home","enabled":true,"network":"People",
	     "security":{"type":"WPA2_PERSONAL","passphraseEnv":"UNIFI_PSK_HOME"},
	     "bands":[2.4,5],"clientIsolationEnabled":false,"hideName":false,
	     "multicastToUnicastConversionEnabled":true,"uapsdEnabled":false}
	  ],
	  "firewallPolicies": [
	    {"name":"things-to-trusted","enabled":true,"action":"BLOCK","allowReturnTraffic":true,
	     "sourceZone":"Things","destinationZone":"Trusted","ipVersion":"IPV4_AND_IPV6","loggingEnabled":false,"order":10}
	  ],
	  "dnsPolicies": []
	}`
}

// A first rollout on a zone-based-firewall console: the console refuses a
// network create without a zoneId, so each new zone is created ahead of its
// networks, each network is created into its zone, and the zone pass then
// finds the membership already in place. Hotspot gains Guest by Guest's
// create, so it reads OK rather than UPDATE.
func TestZonedFirstRollout(t *testing.T) {
	f := newFakeConsole(t)
	seedZonedConsole(f)
	env := []string{"UNIFI_PSK_HOME=correct-horse-battery"}

	lines := `CREATE firewall zone  Trusted (1 networks)
CREATE firewall zone  Things (1 networks)
OK     network        Default
CREATE network        People (vlan 10, zone Trusted)
CREATE network        IoT (vlan 20, zone Things)
CREATE network        Guest (vlan 50, zone Hotspot)
OK     firewall zone  Internal
OK     firewall zone  Hotspot
CREATE wifi           home (WPA2_PERSONAL on People)
CREATE firewall policy Things -> Trusted / things-to-trusted (BLOCK)
`
	plan, _, code := run(t, f, zonedSite(), env, "diff")
	if code != exitChangesPending || plan != "DRY RUN — no changes will be made\n"+lines {
		t.Fatalf("diff exited %d with plan:\n%s\nwant:\n%s", code, plan, lines)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Fatalf("diff wrote to the console: %+v", muts)
	}

	// The writing pass prints the plan it carries out, and it is the same.
	if out := mustRun(t, f, zonedSite(), env, "sync"); out != lines {
		t.Errorf("sync printed:\n%s\nwant the dry-run plan:\n%s", out, lines)
	}

	var order []string
	for _, m := range f.recorded() {
		if m.Method == "POST" {
			order = append(order, m.Path+" "+m.Body["name"].(string))
		}
	}
	want := []string{
		"firewall/zones Trusted", "firewall/zones Things",
		"networks People", "networks IoT", "networks Guest",
		"wifi/broadcasts home", "firewall/policies things-to-trusted",
	}
	if !equalStrings(order, want) {
		t.Errorf("creates in order %v, want %v", order, want)
	}

	for zone, net := range map[string]string{"Trusted": "People", "Things": "IoT", "Hotspot": "Guest"} {
		z, n := f.objectNamed(collZones, zone), f.objectNamed(collNetworks, net)
		if !equalStrings(zoneMembers(z), []string{n["id"].(string)}) || n["zoneId"] != z["id"] {
			t.Errorf("zone %s holds %v, network %s has zoneId %v", zone, zoneMembers(z), net, n["zoneId"])
		}
	}

	again, _, code := run(t, f, zonedSite(), env, "diff")
	if code != 0 {
		t.Errorf("diff after the sync exited %d, want a clean no-op:\n%s", code, again)
	}
}

// Creating a zone that lists an existing network moves that network out of
// its old zone. The dry run cannot see the move, so it must not plan an
// update for the old zone that the writing pass would find unnecessary; the
// move still counts towards --max-changes.
func TestZonedCreateMovesExistingNetwork(t *testing.T) {
	f := newFakeConsole(t)
	seedZonedConsole(f)
	env := []string{"UNIFI_PSK_HOME=correct-horse-battery"}
	mustRun(t, f, zonedSite(), env, "sync")

	// IoT also changes, so its network update must carry the new zone's id
	// rather than the one read before the zone was created.
	moved := strings.Replace(zonedSite(),
		`{"name":"Things","networks":["IoT"]},`,
		`{"name":"Things","networks":[]}, {"name":"Devices","networks":["IoT"]},`, 1)
	iotNet := zonedNetwork("IoT", 20, "10.0.20")
	moved = strings.Replace(moved, iotNet, strings.Replace(iotNet, `"isolationEnabled":false`, `"isolationEnabled":true`, 1), 1)
	lines := `CREATE firewall zone  Devices (1 networks; moves IoT from Things)
OK     network        Default
OK     network        People
UPDATE network        IoT (isolationEnabled)
OK     network        Guest
OK     firewall zone  Internal
OK     firewall zone  Trusted
OK     firewall zone  Things
OK     firewall zone  Hotspot
OK     wifi           home
OK     firewall policy Things -> Trusted / things-to-trusted
`
	if plan, _, _ := run(t, f, moved, env, "diff"); plan != "DRY RUN — no changes will be made\n"+lines {
		t.Fatalf("diff planned:\n%s\nwant:\n%s", plan, lines)
	}
	writes := len(f.recorded())
	if _, stderr, code := run(t, f, moved, env, "sync", "--max-changes", "0"); code != 1 ||
		!strings.Contains(stderr, "(0 deleted, 2 updated, 0 firewall policies moved)") {
		t.Fatalf("sync --max-changes 0 exited %d, want a refusal counting the move:\n%s", code, stderr)
	}
	if muts := f.recorded()[writes:]; len(muts) != 0 {
		t.Fatalf("a refused sync wrote to the console: %+v", muts)
	}
	if out := mustRun(t, f, moved, env, "sync", "--max-changes", "2"); out != lines {
		t.Errorf("sync printed:\n%s\nwant the dry-run plan:\n%s", out, lines)
	}
	iot, devices := f.objectNamed(collNetworks, "IoT"), f.objectNamed(collZones, "Devices")
	if iot["zoneId"] != devices["id"] || !equalStrings(zoneMembers(devices), []string{iot["id"].(string)}) {
		t.Errorf("IoT has zoneId %v, Devices holds %v", iot["zoneId"], zoneMembers(devices))
	}
	if again, _, code := run(t, f, moved, env, "diff"); code != 0 {
		t.Errorf("diff after the sync exited %d, want a clean no-op:\n%s", code, again)
	}
}

// The console needs exactly one zone for each network it creates, so an
// instance file that gives a new network none, or two, is refused before
// anything is written.
func TestZonedNetworkNeedsExactlyOneZone(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, err string }{
		"no zone": {
			`{"name":"Things","networks":["IoT"]}`, `{"name":"Things","networks":[]}`,
			`network "IoT" is in no declared firewall zone`,
		},
		"two zones": {
			`{"name":"Things","networks":["IoT"]}`, `{"name":"Things","networks":["IoT","Guest"]}`,
			`network "Guest" is declared in firewall zones "Things" and "Hotspot"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeConsole(t)
			seedZonedConsole(f)
			env := []string{"UNIFI_PSK_HOME=correct-horse-battery"}
			desired := strings.Replace(zonedSite(), tc.from, tc.to, 1)
			for _, args := range [][]string{{"diff"}, {"sync"}} {
				_, stderr, code := run(t, f, desired, env, args...)
				if code != 1 || !strings.Contains(stderr, tc.err) {
					t.Errorf("%v exited %d, want 1 with %q:\n%s", args, code, tc.err, stderr)
				}
			}
			if muts := f.recorded(); len(muts) != 0 {
				t.Errorf("a refused instance file wrote to the console: %+v", muts)
			}
		})
	}
}

// Without the zone-based firewall there are no zones to join: a network is
// created without a zoneId, as the legacy console expects.
func TestUnzonedNetworkCreate(t *testing.T) {
	f := newFakeConsole(t)
	f.zbfConfigured = false
	desired := `{"networks":[` + zonedNetwork("People", 10, "10.0.10") + `]}`
	mustRun(t, f, desired, nil, "sync")
	muts := f.recorded()
	if len(muts) != 1 || muts[0].Method != "POST" || muts[0].Body["name"] != "People" {
		t.Fatalf("writes = %+v, want one network POST", muts)
	}
	if _, ok := muts[0].Body["zoneId"]; ok {
		t.Errorf("network create carried a zoneId without the zone-based firewall: %v", muts[0].Body)
	}
}

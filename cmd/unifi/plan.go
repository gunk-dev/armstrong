package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// reconciler converges one site to the desired #Site document. Resource types
// are handled in dependency order: networks, then firewall zones (which
// reference networks), then wifi / firewall policies / DNS policies.
type reconciler struct {
	client  *client
	siteID  string
	want    site
	prune   bool
	dryRun  bool
	out     io.Writer
	changed bool

	networkIDs map[string]string // network name -> id
	zoneIDs    map[string]string // zone name -> id
}

// pendingID stands in for an id that would only exist after an earlier create
// in this same run. Under --dry-run nothing is created, so references to a
// brand-new object cannot be resolved.
const pendingID = "<pending>"

// newID is the id to remember for a just-created object: the one the console
// assigned, or the placeholder when --dry-run meant nothing was created.
func newID(created string, dryRun bool) string {
	if dryRun {
		return pendingID
	}
	return created
}

func (r *reconciler) logf(verb, kind, name, format string, args ...any) {
	if verb != "OK" {
		r.changed = true
	}
	detail := fmt.Sprintf(format, args...)
	if detail != "" {
		detail = " (" + detail + ")"
	}
	fmt.Fprintf(r.out, "%-6s %-14s %s%s\n", verb, kind, name, redact(detail))
}

// mutate performs a write unless this is a dry run.
func (r *reconciler) mutate(method, path string, body any, out any) error {
	if r.dryRun {
		return nil
	}
	return r.client.do(method, path, body, out)
}

func (r *reconciler) run() error {
	if r.dryRun {
		fmt.Fprintln(r.out, "DRY RUN — no changes will be made")
	}
	for _, step := range []func() error{
		r.syncNetworks,
		r.syncZones,
		r.syncWiFi,
		r.syncFirewallPolicies,
		r.syncDNSPolicies,
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- networks

func (r *reconciler) syncNetworks() error {
	existing, err := r.client.networks(r.siteID)
	if err != nil {
		return err
	}
	r.networkIDs = map[string]string{}
	byName := map[string]actual[network]{}
	for _, a := range existing {
		byName[a.Spec.Name] = a
		r.networkIDs[a.Spec.Name] = a.ID
	}

	seen := map[string]bool{}
	base := "/sites/" + r.siteID + "/networks"
	for _, want := range r.want.Networks {
		seen[want.Name] = true
		got, ok := byName[want.Name]
		if !ok {
			r.logf("CREATE", "network", want.Name, "vlan %d", want.VlanID)
			var created apiNetwork
			if err := r.mutate(http.MethodPost, base, want.body(), &created); err != nil {
				return fmt.Errorf("create network %q: %w", want.Name, err)
			}
			r.networkIDs[want.Name] = newID(created.ID, r.dryRun)
			continue
		}
		if reflect.DeepEqual(normalizeNetwork(got.Spec), normalizeNetwork(want)) {
			r.logf("OK", "network", want.Name, "")
			continue
		}
		r.logf("UPDATE", "network", want.Name, "%s", diffSummary(got.Spec, want))
		if err := r.mutate(http.MethodPut, base+"/"+got.ID, want.body(), nil); err != nil {
			return fmt.Errorf("update network %q: %w", want.Name, err)
		}
	}

	return r.pruneList("network", base, len(r.want.Networks) > 0, func(yield func(id, name, origin string)) {
		for _, a := range existing {
			if !seen[a.Spec.Name] {
				yield(a.ID, a.Spec.Name, a.Origin)
			}
		}
	})
}

// normalizeNetwork drops fields the API fills in on its own so that equal
// configurations compare equal.
func normalizeNetwork(n network) network {
	n.Management = "GATEWAY"
	if n.IPv4 != nil && n.IPv4.DHCP != nil && n.IPv4.DHCP.Mode != "SERVER" {
		n.IPv4.DHCP = &networkDHCP{Mode: n.IPv4.DHCP.Mode}
	}
	if n.IPv4 != nil && n.IPv4.DHCP != nil && len(n.IPv4.DHCP.DNSServers) == 0 {
		n.IPv4.DHCP.DNSServers = nil
	}
	return n
}

// ----------------------------------------------------------- firewall zones

func (r *reconciler) syncZones() error {
	existing, available, err := r.client.zones(r.siteID)
	if err != nil {
		return err
	}
	r.zoneIDs = map[string]string{}
	if !available {
		if len(r.want.FirewallZones) > 0 || len(r.want.FirewallPolicies) > 0 {
			r.logf("SKIP", "firewall", "zones+policies", "zone-based firewall is not configured on this console")
		}
		return nil
	}

	byName := map[string]actual[apiZone]{}
	for _, a := range existing {
		byName[a.Spec.Name] = a
		r.zoneIDs[a.Spec.Name] = a.ID
	}

	seen := map[string]bool{}
	base := "/sites/" + r.siteID + "/firewall/zones"
	for _, want := range r.want.FirewallZones {
		seen[want.Name] = true
		ids, err := r.resolveNetworks(want.Networks)
		if err != nil {
			return fmt.Errorf("firewall zone %q: %w", want.Name, err)
		}
		body := map[string]any{"name": want.Name, "networkIds": ids}

		got, ok := byName[want.Name]
		if !ok {
			r.logf("CREATE", "firewall zone", want.Name, "%d networks", len(ids))
			var created apiZone
			if err := r.mutate(http.MethodPost, base, body, &created); err != nil {
				return fmt.Errorf("create firewall zone %q: %w", want.Name, err)
			}
			r.zoneIDs[want.Name] = newID(created.ID, r.dryRun)
			continue
		}
		if sameStringSet(got.Spec.NetworkIDs, ids) {
			r.logf("OK", "firewall zone", want.Name, "")
			continue
		}
		r.logf("UPDATE", "firewall zone", want.Name, "member networks changed")
		if err := r.mutate(http.MethodPut, base+"/"+got.ID, body, nil); err != nil {
			return fmt.Errorf("update firewall zone %q: %w", want.Name, err)
		}
	}

	return r.pruneList("firewall zone", base, len(r.want.FirewallZones) > 0, func(yield func(id, name, origin string)) {
		for _, a := range existing {
			if !seen[a.Spec.Name] {
				yield(a.ID, a.Spec.Name, a.Origin)
			}
		}
	})
}

// ------------------------------------------------------------------- wifi

func (r *reconciler) syncWiFi() error {
	existing, err := r.client.wifis(r.siteID)
	if err != nil {
		return err
	}
	byName := map[string]actual[apiWiFi]{}
	for _, a := range existing {
		byName[a.Spec.Name] = a
	}

	seen := map[string]bool{}
	base := "/sites/" + r.siteID + "/wifi/broadcasts"
	for _, want := range r.want.WiFi {
		seen[want.Name] = true

		passphrase := ""
		if want.Security.PassphraseEnv != "" {
			passphrase = os.Getenv(want.Security.PassphraseEnv)
			if passphrase == "" {
				return fmt.Errorf("wifi %q: environment variable %s is empty", want.Name, want.Security.PassphraseEnv)
			}
			registerSecret(passphrase)
		}
		networkID := ""
		if want.Network != "" && want.Network != "NATIVE" {
			id, ok := r.networkIDs[want.Network]
			if !ok {
				return fmt.Errorf("wifi %q references unknown network %q", want.Name, want.Network)
			}
			networkID = id
		}
		body := want.body(networkID, passphrase)

		got, ok := byName[want.Name]
		if !ok {
			r.logf("CREATE", "wifi", want.Name, "%s on %s", want.Security.Type, displayNetwork(want.Network))
			if err := r.mutate(http.MethodPost, base, body, nil); err != nil {
				return fmt.Errorf("create wifi %q: %w", want.Name, err)
			}
			continue
		}
		if changes := wifiChanges(got.Spec, want, networkID, passphrase); len(changes) == 0 {
			r.logf("OK", "wifi", want.Name, "")
			continue
		} else {
			r.logf("UPDATE", "wifi", want.Name, "%s", strings.Join(changes, ", "))
		}
		if err := r.mutate(http.MethodPut, base+"/"+got.ID, body, nil); err != nil {
			return fmt.Errorf("update wifi %q: %w", want.Name, err)
		}
	}

	return r.pruneList("wifi", base, len(r.want.WiFi) > 0, func(yield func(id, name, origin string)) {
		for _, a := range existing {
			if !seen[a.Spec.Name] {
				yield(a.ID, a.Spec.Name, a.Origin)
			}
		}
	})
}

// wifiChanges lists the field names that differ. The passphrase is compared
// but only ever reported by name.
func wifiChanges(got apiWiFi, want wifi, networkID, passphrase string) []string {
	var changes []string
	add := func(cond bool, field string) {
		if cond {
			changes = append(changes, field)
		}
	}
	add(got.Enabled != want.Enabled, "enabled")
	add(got.SecurityConfiguration.Type != want.Security.Type, "security.type")
	add(want.Security.Type != "OPEN" && got.SecurityConfiguration.Passphrase != passphrase, "security.passphrase")
	add(!sameFloats(got.BroadcastingFrequenciesGHz, want.Bands), "bands")
	add(got.ClientIsolationEnabled != want.ClientIsolationEnabled, "clientIsolationEnabled")
	add(got.HideName != want.HideName, "hideName")
	add(got.MulticastToUnicastConversionEnabled != want.MulticastToUnicastConversionEnabled, "multicastToUnicastConversionEnabled")
	add(got.UAPSDEnabled != want.UAPSDEnabled, "uapsdEnabled")

	wantNetworkType, wantNetworkID := "NATIVE", ""
	if networkID != "" {
		wantNetworkType, wantNetworkID = "SPECIFIC", networkID
	}
	add(got.Network.Type != wantNetworkType || got.Network.NetworkID != wantNetworkID, "network")

	for field, pair := range map[string][2]*bool{
		"bandSteeringEnabled":  {got.BandSteeringEnabled, want.BandSteeringEnabled},
		"arpProxyEnabled":      {got.ARPProxyEnabled, want.ARPProxyEnabled},
		"bssTransitionEnabled": {got.BSSTransitionEnabled, want.BSSTransitionEnabled},
		"advertiseDeviceName":  {got.AdvertiseDeviceName, want.AdvertiseDeviceName},
	} {
		add(pair[1] != nil && (pair[0] == nil || *pair[0] != *pair[1]), field)
	}
	sort.Strings(changes)
	return changes
}

// --------------------------------------------------------- firewall policies

func (r *reconciler) syncFirewallPolicies() error {
	if len(r.zoneIDs) == 0 && len(r.want.FirewallPolicies) == 0 {
		return nil
	}
	existing, available, err := r.client.firewallPolicies(r.siteID)
	if err != nil {
		return err
	}
	if !available {
		return nil
	}

	byName := map[string]actual[apiFirewallPolicy]{}
	for _, a := range existing {
		byName[a.Spec.Name] = a
	}

	// Desired policies run in `order` sequence, ahead of the system-defined ones.
	wanted := append([]firewallPolicy(nil), r.want.FirewallPolicies...)
	sort.SliceStable(wanted, func(i, j int) bool { return wanted[i].Order < wanted[j].Order })

	seen := map[string]bool{}
	base := "/sites/" + r.siteID + "/firewall/policies"
	var ordered []string
	for _, want := range wanted {
		seen[want.Name] = true
		srcZone, ok := r.zoneIDs[want.SourceZone]
		if !ok {
			return fmt.Errorf("firewall policy %q references unknown zone %q", want.Name, want.SourceZone)
		}
		dstZone, ok := r.zoneIDs[want.DestinationZone]
		if !ok {
			return fmt.Errorf("firewall policy %q references unknown zone %q", want.Name, want.DestinationZone)
		}
		srcNets, err := r.resolveNetworks(want.SourceNetworks)
		if err != nil {
			return fmt.Errorf("firewall policy %q: %w", want.Name, err)
		}
		dstNets, err := r.resolveNetworks(want.DestinationNetworks)
		if err != nil {
			return fmt.Errorf("firewall policy %q: %w", want.Name, err)
		}
		body, err := want.body(srcZone, dstZone, srcNets, dstNets)
		if err != nil {
			return fmt.Errorf("firewall policy %q: %w", want.Name, err)
		}

		got, ok := byName[want.Name]
		if !ok {
			r.logf("CREATE", "firewall policy", want.Name, "%s %s -> %s", want.Action, want.SourceZone, want.DestinationZone)
			var created apiFirewallPolicy
			if err := r.mutate(http.MethodPost, base, body, &created); err != nil {
				return fmt.Errorf("create firewall policy %q: %w", want.Name, err)
			}
			ordered = append(ordered, newID(created.ID, r.dryRun))
			continue
		}
		ordered = append(ordered, got.ID)
		if policyMatches(got.Spec, want, srcZone, dstZone) {
			r.logf("OK", "firewall policy", want.Name, "")
			continue
		}
		r.logf("UPDATE", "firewall policy", want.Name, "%s %s -> %s", want.Action, want.SourceZone, want.DestinationZone)
		if err := r.mutate(http.MethodPut, base+"/"+got.ID, body, nil); err != nil {
			return fmt.Errorf("update firewall policy %q: %w", want.Name, err)
		}
	}

	if err := r.pruneList("firewall policy", base, len(r.want.FirewallPolicies) > 0, func(yield func(id, name, origin string)) {
		for _, a := range existing {
			if !seen[a.Spec.Name] {
				yield(a.ID, a.Spec.Name, a.Origin)
			}
		}
	}); err != nil {
		return err
	}

	return r.reorderPolicies(base, ordered, existing)
}

// reorderPolicies puts the managed policies in `order` sequence ahead of the
// console's system-defined ones. The list endpoint returns policies in
// evaluation order, so the current sequence is compared first and the write is
// skipped when it already matches — otherwise every sync would issue one.
func (r *reconciler) reorderPolicies(base string, ordered []string, existing []actual[apiFirewallPolicy]) error {
	if len(ordered) < 2 {
		return nil
	}
	managed := map[string]bool{}
	for _, id := range ordered {
		managed[id] = true
	}
	var current []string
	for _, a := range existing {
		if managed[a.ID] {
			current = append(current, a.ID)
		}
	}
	if slices.Equal(current, ordered) {
		return nil
	}

	r.logf("ORDER", "firewall policy", fmt.Sprintf("%d policies", len(ordered)), "")
	body := map[string]any{"orderedFirewallPolicyIds": map[string]any{
		"beforeSystemDefined": ordered,
		"afterSystemDefined":  []string{},
	}}
	if err := r.mutate(http.MethodPut, base+"/ordering", body, nil); err != nil {
		return fmt.Errorf("reorder firewall policies: %w", err)
	}
	return nil
}

func policyMatches(got apiFirewallPolicy, want firewallPolicy, srcZone, dstZone string) bool {
	if got.Enabled != want.Enabled || got.Action.Type != want.Action || got.LoggingEnabled != want.LoggingEnabled {
		return false
	}
	if want.Action == "ALLOW" && got.Action.AllowReturnTraffic != want.AllowReturnTraffic {
		return false
	}
	if got.Source.ZoneID != srcZone || got.Destination.ZoneID != dstZone {
		return false
	}
	if got.IPProtocolScope.IPVersion != want.IPVersion {
		return false
	}
	gotProtocol := ""
	if f := got.IPProtocolScope.ProtocolFilter; f != nil {
		gotProtocol = f.Name
	}
	if gotProtocol != want.Protocol {
		return false
	}
	return sameStringSet(got.ConnectionStateFilter, want.ConnectionStates)
}

// -------------------------------------------------------------- dns policies

func (r *reconciler) syncDNSPolicies() error {
	existing, err := r.client.dnsPolicies(r.siteID)
	if err != nil {
		return err
	}
	byKey := map[string]actual[dnsPolicy]{}
	for _, a := range existing {
		byKey[a.Spec.key()] = a
	}

	seen := map[string]bool{}
	base := "/sites/" + r.siteID + "/dns/policies"
	for _, want := range r.want.DNSPolicies {
		seen[want.key()] = true
		got, ok := byKey[want.key()]
		if !ok {
			r.logf("CREATE", "dns policy", want.key(), "")
			if err := r.mutate(http.MethodPost, base, want.body(), nil); err != nil {
				return fmt.Errorf("create dns policy %q: %w", want.key(), err)
			}
			continue
		}
		if reflect.DeepEqual(normalizeDNSPolicy(got.Spec), normalizeDNSPolicy(want)) {
			r.logf("OK", "dns policy", want.key(), "")
			continue
		}
		r.logf("UPDATE", "dns policy", want.key(), "")
		if err := r.mutate(http.MethodPut, base+"/"+got.ID, want.body(), nil); err != nil {
			return fmt.Errorf("update dns policy %q: %w", want.key(), err)
		}
	}

	return r.pruneList("dns policy", base, len(r.want.DNSPolicies) > 0, func(yield func(id, name, origin string)) {
		for _, a := range existing {
			if !seen[a.Spec.key()] {
				yield(a.ID, a.Spec.key(), a.Origin)
			}
		}
	})
}

// normalizeDNSPolicy fills in the TTL the API defaults to, so that a record
// whose instance file omits ttlSeconds does not look changed on every run.
func normalizeDNSPolicy(d dnsPolicy) dnsPolicy {
	if d.TTLSeconds == nil {
		zero := 0
		d.TTLSeconds = &zero
	}
	return d
}

// ------------------------------------------------------------------ helpers

// pruneList deletes unmatched USER_DEFINED objects when --prune is set.
//
// Two safety rules apply. SYSTEM_DEFINED objects are never deleted, prune or
// not — they are the console's own. And nothing is deleted for a resource type
// the instance file leaves empty (declared is false): an instance file that
// simply forgot a list would otherwise wipe every object of that type.
func (r *reconciler) pruneList(kind, base string, declared bool, each func(func(id, name, origin string))) error {
	var err error
	each(func(id, name, origin string) {
		if err != nil || origin == originSystem {
			return
		}
		if !r.prune || !declared {
			return
		}
		r.logf("DELETE", kind, name, "")
		if derr := r.mutate(http.MethodDelete, base+"/"+id, nil, nil); derr != nil {
			err = fmt.Errorf("delete %s %q: %w", kind, name, derr)
		}
	})
	return err
}

func (r *reconciler) resolveNetworks(names []string) ([]string, error) {
	ids := make([]string, 0, len(names))
	for _, n := range names {
		id, ok := r.networkIDs[n]
		if !ok {
			return nil, fmt.Errorf("unknown network %q", n)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func displayNetwork(n string) string {
	if n == "" {
		return "NATIVE"
	}
	return n
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

func sameFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]float64(nil), a...)
	y := append([]float64(nil), b...)
	sort.Float64s(x)
	sort.Float64s(y)
	return reflect.DeepEqual(x, y)
}

// diffSummary names the top-level network fields that differ, for the plan output.
func diffSummary(got, want network) string {
	var fields []string
	if got.Enabled != want.Enabled {
		fields = append(fields, "enabled")
	}
	if got.VlanID != want.VlanID {
		fields = append(fields, "vlanId")
	}
	if got.IsolationEnabled != want.IsolationEnabled {
		fields = append(fields, "isolationEnabled")
	}
	if got.InternetAccessEnabled != want.InternetAccessEnabled {
		fields = append(fields, "internetAccessEnabled")
	}
	if got.CellularBackupEnabled != want.CellularBackupEnabled {
		fields = append(fields, "cellularBackupEnabled")
	}
	if got.MDNSForwardingEnabled != want.MDNSForwardingEnabled {
		fields = append(fields, "mdnsForwardingEnabled")
	}
	if !reflect.DeepEqual(normalizeNetwork(got).IPv4, normalizeNetwork(want).IPv4) {
		fields = append(fields, "ipv4")
	}
	if len(fields) == 0 {
		return "changed"
	}
	return strings.Join(fields, ", ")
}

func splitPortRange(s string) (start, end int, ok bool) {
	i := strings.Index(s, "-")
	if i <= 0 {
		return 0, 0, false
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(s[:i]))
	b, err2 := strconv.Atoi(strings.TrimSpace(s[i+1:]))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return a, b, true
}

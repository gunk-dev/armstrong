package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
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

	netNames := nameLookup{}
	for name, id := range r.networkIDs {
		netNames[id] = name
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
		// A zone whose members GET /networks does not return (the console's
		// `External` zone lists WAN interfaces) cannot be written back: the
		// PUT would carry only the members the tool could name.
		if _, ok := zoneNetworkNames(got.Spec, netNames); !ok {
			return fmt.Errorf("firewall zone %q has member networks that GET /networks does not "+
				"return (WAN interfaces); updating it would drop them. Remove it from the "+
				"instance file — policies can reference it by name without declaring it", want.Name)
		}
		if !got.Configurable {
			return fmt.Errorf("firewall zone %q is reported as not configurable by the console; "+
				"it cannot be changed", want.Name)
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

	zoneNames := nameLookup{}
	for name, id := range r.zoneIDs {
		zoneNames[id] = name
	}
	netNames := nameLookup{}
	for name, id := range r.networkIDs {
		netNames[id] = name
	}

	byKey := map[string]actual[apiFirewallPolicy]{}
	for _, a := range existing {
		k := a.Spec.spec(zoneNames, netNames).key()
		if _, dup := byKey[k]; dup {
			return fmt.Errorf("the console holds two firewall policies with the same identity (%s); "+
				"cmd/unifi cannot tell them apart — rename one in the console first", k)
		}
		byKey[k] = a
	}

	if err := checkDuplicatePolicyKeys(r.want.FirewallPolicies); err != nil {
		return err
	}

	seen := map[string]bool{}
	base := "/sites/" + r.siteID + "/firewall/policies"
	// managed records, per zone pair, the policies this run is responsible for
	// ordering.
	managed := map[string][]managedPolicy{}
	for _, want := range r.want.FirewallPolicies {
		seen[want.key()] = true
		body, err := want.body(r.resolveZone, r.resolveNetwork)
		if err != nil {
			return fmt.Errorf("firewall policy %q: %w", want.key(), err)
		}

		got, ok := byKey[want.key()]
		if !ok {
			r.logf("CREATE", "firewall policy", want.key(), "%s", want.Action)
			var created apiFirewallPolicy
			if err := r.mutate(http.MethodPost, base, body, &created); err != nil {
				return fmt.Errorf("create firewall policy %q: %w", want.key(), err)
			}
			managed[want.zonePair()] = append(managed[want.zonePair()], managedPolicy{
				key: want.key(), id: newID(created.ID, r.dryRun),
				srcZone: want.SourceZone, dstZone: want.DestinationZone,
				order: want.Order, system: false,
			})
			continue
		}
		managed[want.zonePair()] = append(managed[want.zonePair()], managedPolicy{
			key: want.key(), id: got.ID,
			srcZone: want.SourceZone, dstZone: want.DestinationZone,
			order: want.Order, system: got.IsSystem,
		})

		// Never plan a write against a live object the schema cannot express:
		// the PUT would drop whatever it failed to read.
		if lossy := policyUnmodelledFields(got, zoneNames, netNames); len(lossy) > 0 {
			return fmt.Errorf("firewall policy %q uses fields schema/unifi.cue does not model (%s); "+
				"managing it would rewrite the policy without them. Remove it from the instance "+
				"file, or extend #FirewallPolicy — see docs/unifi-api-notes.md",
				want.key(), strings.Join(lossy, ", "))
		}
		if policyMatches(got.Spec.spec(zoneNames, netNames), want) {
			r.logf("OK", "firewall policy", want.key(), "")
			continue
		}
		if got.IsSystem && !got.Configurable {
			return fmt.Errorf("firewall policy %q is SYSTEM_DEFINED and the console reports it as "+
				"not configurable; it cannot be changed", want.key())
		}
		if got.ID == "" {
			return errNoPolicyID(want.key(), "update")
		}
		r.logf("UPDATE", "firewall policy", want.key(), "%s", policyChanges(got.Spec.spec(zoneNames, netNames), want))
		if err := r.mutate(http.MethodPut, base+"/"+got.ID, body, nil); err != nil {
			return fmt.Errorf("update firewall policy %q: %w", want.key(), err)
		}
	}

	if err := r.prunePolicies(base, existing, seen, zoneNames, netNames); err != nil {
		return err
	}

	return r.reorderPolicies(base, managed, existing, zoneNames, netNames)
}

// managedPolicy is one policy this run owns, with everything the ordering step
// needs to decide whether the console already agrees.
type managedPolicy struct {
	key     string
	id      string
	srcZone string
	dstZone string
	order   *int
	system  bool
}

// errNoPolicyID explains the firmware limitation that makes a write
// impossible: UniFi Network 10.6 omits `id` from USER_DEFINED firewall
// policies, so there is no URL to address them at.
func errNoPolicyID(key, verb string) error {
	return fmt.Errorf("cannot %s firewall policy %q: the console returned it without an id, which "+
		"UniFi Network 10.6 does for every USER_DEFINED policy, so there is no endpoint to "+
		"address it at. Change it in the console UI, or delete and re-create it so that "+
		"cmd/unifi owns it — see docs/unifi-api-notes.md", verb, key)
}

func checkDuplicatePolicyKeys(policies []firewallPolicy) error {
	seen := map[string]bool{}
	for _, p := range policies {
		if seen[p.key()] {
			return fmt.Errorf("two firewall policies share the identity (%s); a policy is keyed by "+
				"source zone, destination zone and name, so these cannot both be reconciled", p.key())
		}
		seen[p.key()] = true
	}
	return nil
}

// prunePolicies deletes undeclared USER_DEFINED policies. It cannot use
// pruneList: policies are keyed by a triple rather than by name, and an
// id-less policy has to fail loudly rather than be skipped.
func (r *reconciler) prunePolicies(base string, existing []actual[apiFirewallPolicy], seen map[string]bool, zoneNames, netNames nameLookup) error {
	if !r.prune || len(r.want.FirewallPolicies) == 0 {
		return nil
	}
	for _, a := range existing {
		key := a.Spec.spec(zoneNames, netNames).key()
		if seen[key] || a.Origin == originSystem {
			continue
		}
		if a.ID == "" {
			return errNoPolicyID(key, "delete")
		}
		r.logf("DELETE", "firewall policy", key, "")
		if err := r.mutate(http.MethodDelete, base+"/"+a.ID, nil, nil); err != nil {
			return fmt.Errorf("delete firewall policy %q: %w", key, err)
		}
	}
	return nil
}

// reorderPolicies puts the managed USER_DEFINED policies of each zone pair in
// `order` sequence. The console orders policies per zone pair, so the ordering
// endpoint takes the pair as query parameters and there is no site-wide call.
//
// The current sequence is compared first and the write skipped when it already
// matches — otherwise every sync would issue one. The comparison is by policy
// key rather than by id, because a USER_DEFINED policy has no id to compare.
func (r *reconciler) reorderPolicies(base string, managed map[string][]managedPolicy, existing []actual[apiFirewallPolicy], zoneNames, netNames nameLookup) error {
	for _, pair := range sortedKeys(managed) {
		policies := managed[pair]
		// Only USER_DEFINED policies move; the console pins its own.
		var movable []managedPolicy
		ordered := false
		for _, p := range policies {
			if p.system {
				continue
			}
			movable = append(movable, p)
			ordered = ordered || p.order != nil
		}
		if !ordered || len(movable) < 2 {
			continue
		}
		sort.SliceStable(movable, func(i, j int) bool {
			return orderOf(movable[i].order) < orderOf(movable[j].order)
		})

		wantKeys := make([]string, 0, len(movable))
		for _, p := range movable {
			wantKeys = append(wantKeys, p.key)
		}
		inPair := map[string]bool{}
		for _, k := range wantKeys {
			inPair[k] = true
		}
		// existing is in evaluation order, so filtering it yields the console's
		// current sequence for this pair.
		var currentKeys []string
		for _, a := range existing {
			if k := a.Spec.spec(zoneNames, netNames).key(); inPair[k] {
				currentKeys = append(currentKeys, k)
			}
		}
		if slices.Equal(currentKeys, wantKeys) {
			continue
		}

		ids := make([]string, 0, len(movable))
		for _, p := range movable {
			if p.id == "" {
				return errNoPolicyID(p.key, "reorder")
			}
			ids = append(ids, p.id)
		}
		r.logf("ORDER", "firewall policy", pair, "%d policies", len(ids))

		srcZone, err := r.resolveZone(movable[0].srcZone)
		if err != nil {
			return err
		}
		dstZone, err := r.resolveZone(movable[0].dstZone)
		if err != nil {
			return err
		}
		body := map[string]any{"orderedFirewallPolicyIds": map[string]any{
			"beforeSystemDefined": ids,
			"afterSystemDefined":  []string{},
		}}
		path := fmt.Sprintf("%s/ordering?sourceFirewallZoneId=%s&destinationFirewallZoneId=%s",
			base, url.QueryEscape(srcZone), url.QueryEscape(dstZone))
		if err := r.mutate(http.MethodPut, path, body, nil); err != nil {
			return fmt.Errorf("reorder firewall policies %s: %w", pair, err)
		}
	}
	return nil
}

// orderOf sorts unordered policies after ordered ones, keeping their relative
// position among themselves.
func orderOf(o *int) int {
	if o == nil {
		return math.MaxInt
	}
	return *o
}

// policyMatches compares a live policy's projection against the desired one.
// `order` is deliberately excluded: position is reconciled separately, by the
// ordering endpoint.
func policyMatches(got, want firewallPolicy) bool {
	return reflect.DeepEqual(normalizePolicy(got), normalizePolicy(want))
}

// normalizePolicy drops the differences that are not differences: the position
// (reconciled by the ordering endpoint), the order of a set-valued list, and
// allowReturnTraffic on an action that has no reply traffic to allow — the API
// neither stores nor returns it there, so the schema default must not read as
// drift on every run.
func normalizePolicy(p firewallPolicy) firewallPolicy {
	p.Order = nil
	p.ConnectionStates = sortedCopy(p.ConnectionStates)
	if p.Action != "ALLOW" {
		p.AllowReturnTraffic = false
	}
	return p
}

// policyChanges names the parts of the policy that differ, for the plan output.
func policyChanges(got, want firewallPolicy) string {
	var fields []string
	add := func(cond bool, field string) {
		if cond {
			fields = append(fields, field)
		}
	}
	add(got.Enabled != want.Enabled, "enabled")
	add(got.Description != want.Description, "description")
	add(got.Action != want.Action || normalizePolicy(got).AllowReturnTraffic != normalizePolicy(want).AllowReturnTraffic, "action")
	add(!reflect.DeepEqual(got.Source, want.Source), "source")
	add(!reflect.DeepEqual(got.Destination, want.Destination), "destination")
	add(got.IPVersion != want.IPVersion, "ipVersion")
	add(got.Protocol != want.Protocol || got.ProtocolMatchOpposite != want.ProtocolMatchOpposite, "protocol")
	add(!sameStringSet(got.ConnectionStates, want.ConnectionStates), "connectionStates")
	add(got.LoggingEnabled != want.LoggingEnabled, "loggingEnabled")
	add(!reflect.DeepEqual(got.Schedule, want.Schedule), "schedule")
	if len(fields) == 0 {
		return "changed"
	}
	return strings.Join(fields, ", ")
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

// resolveZone turns a zone name into the id the API wants. Zones are resolved
// against the live console, so a policy may reference a zone the instance file
// does not declare.
func (r *reconciler) resolveZone(name string) (string, error) {
	id, ok := r.zoneIDs[name]
	if !ok {
		return "", fmt.Errorf("unknown firewall zone %q", name)
	}
	return id, nil
}

func (r *reconciler) resolveNetwork(name string) (string, error) {
	id, ok := r.networkIDs[name]
	if !ok {
		return "", fmt.Errorf("unknown network %q", name)
	}
	return id, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedCopy(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
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

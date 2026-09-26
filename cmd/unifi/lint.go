package main

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// Console validation rules, checked against a #Site document without
// contacting the console. Every rule here was found by a write a live console
// refused (UniFi Network 10.6.106); each violation names the object, the rule,
// and the error code the console would answer, so that `unifi lint`, `diff`,
// `sync` and `restore` fail before any write instead of halfway through one.
// docs/unifi-api-notes.md records the request shape behind each rule.
//
// schema/unifi.cue encodes the same rules where CUE can express them, as a
// convenience for `cue vet`; this file is the source of truth.

// lintViolation is one broken rule: which object, which rule, and what the
// console would answer.
type lintViolation struct {
	object string // e.g. `firewall policy "a -> b / name"`
	rule   string // short rule name, stable for tests and grep
	detail string
	code   string // the console error code the rule prevents
}

func (v lintViolation) Error() string {
	return fmt.Sprintf("%s: %s: %s (the console answers 400 %s)", v.object, v.rule, v.detail, v.code)
}

// lintSite checks want against every known console rule and returns every
// violation, not only the first.
func lintSite(want site) []error {
	var errs []error
	errs = append(errs, lintZones(want)...)
	for _, p := range want.FirewallPolicies {
		errs = append(errs, lintPolicy(p)...)
	}
	return errs
}

// lintError joins lintSite's violations into one error, or returns nil.
func lintError(want site) error {
	errs := lintSite(want)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("the instance file breaks %d console validation rule(s); nothing was sent:\n%w",
		len(errs), errors.Join(errs...))
}

// lintZones: the console places each network in exactly one zone, and on a
// zone-based-firewall console creates a network only into one (POST /networks
// without a zoneId answers api.network.validation.missing-zone-id). So a
// network is declared in at most one zone, and, once the document declares
// any zone, in exactly one. Whether the console has the zone-based firewall at
// all is checked in the planning pass, which needs the console.
func lintZones(want site) []error {
	var errs []error
	zoneOf := map[string]string{}
	for _, z := range want.FirewallZones {
		for _, n := range z.Networks {
			if other, ok := zoneOf[n]; ok && other != z.Name {
				errs = append(errs, lintViolation{
					object: fmt.Sprintf("network %q", n),
					rule:   "network-in-one-zone",
					detail: fmt.Sprintf("declared in firewall zones %q and %q; the console places each network in exactly one zone", other, z.Name),
					code:   codeMissingZoneID,
				})
				continue
			}
			zoneOf[n] = z.Name
		}
	}
	if len(want.FirewallZones) == 0 {
		return errs
	}
	for _, n := range want.Networks {
		if zoneOf[n.Name] == "" {
			errs = append(errs, lintViolation{
				object: fmt.Sprintf("network %q", n.Name),
				rule:   "network-needs-zone",
				detail: "in no declared firewall zone; the console creates a network only into a zone, so list it in exactly one firewallZones entry's networks",
				code:   codeMissingZoneID,
			})
		}
	}
	return errs
}

// returnTrafficZones are the console's built-in zones whose policies cannot
// allow return traffic, in either direction, whatever else the policy says.
var returnTrafficZones = map[string]bool{"Gateway": true, "External": true}

// consolePolicyNames are the names the console gives its own SYSTEM_DEFINED
// policies that allow return traffic where a created policy may not. `unifi
// export` includes them, so the return-traffic rules skip them: a snapshot
// must pass lint for `restore` to apply it. A "<name> (Return)" policy is the
// one the console derives from a policy that allows return traffic.
var consolePolicyNames = map[string]bool{
	"Allow All Traffic": true, "Allow Return Traffic": true,
	"Allow DHCP": true, "Allow DHCPv6": true, "Allow Link-Local DHCPv6": true,
	"Allow DNS": true, "Allow Public DNS": true, "Allow mDNS": true,
	"Allow ICMP": true, "Allow ICMPv6": true,
	"Allow Neighbor Advertisements": true, "Allow Neighbor Solicitations": true,
	"Allow Router Advertisements": true,
	"Allow Hotspot Portal":        true, "Allow Hotspot Portal Authentication": true,
	"Allow Hotspot Portal Redirects": true,
}

func consoleOwnedPolicy(p firewallPolicy) bool {
	return consolePolicyNames[p.Name] || strings.HasSuffix(p.Name, " (Return)")
}

// protocolIPVersion is the only ipVersion the console takes each ICMP
// protocol with; any other answers api.request.unknown-type-id.
var protocolIPVersion = map[string]string{"ICMP": "IPV4", "ICMPV6": "IPV6"}

func lintPolicy(p firewallPolicy) []error {
	var errs []error
	fail := func(rule, code, format string, args ...any) {
		errs = append(errs, lintViolation{
			object: fmt.Sprintf("firewall policy %q", p.key()),
			rule:   rule, detail: fmt.Sprintf(format, args...), code: code,
		})
	}

	if p.Protocol != "" {
		if _, err := protocolFilter(p.Protocol, p.ProtocolMatchOpposite); err != nil {
			fail("tcp-udp-not-negated", codeUnknownProperty, "%v", err)
		}
	}
	if want, ok := protocolIPVersion[p.Protocol]; ok && p.IPVersion != want {
		fail("icmp-ip-version", codeUnknownTypeID,
			"protocol %s is only accepted with ipVersion %s, not %s; declare one policy per IP version",
			p.Protocol, want, p.IPVersion)
	}

	if p.Action == "ALLOW" && p.AllowReturnTraffic && !consoleOwnedPolicy(p) {
		if onlyReturnStates(p.ConnectionStates) {
			fail("return-traffic-states", codeCantAllowReturnTraffic,
				"allowReturnTraffic on a policy matching only ESTABLISHED and RELATED connections, "+
					"which are the return traffic itself; set allowReturnTraffic: false")
		}
		for _, z := range []string{p.SourceZone, p.DestinationZone} {
			if returnTrafficZones[z] {
				fail("return-traffic-zone", codeCantAllowReturnTraffic,
					"allowReturnTraffic on a policy to or from the built-in %s zone; set allowReturnTraffic: false", z)
				break
			}
		}
	}

	for _, end := range []struct {
		name string
		f    *trafficFilter
	}{{"source", p.Source}, {"destination", p.Destination}} {
		if end.f == nil || end.f.IPAddressFilter == nil {
			continue
		}
		for _, it := range end.f.IPAddressFilter.Items {
			if !ipMatchesVersion(it.Value, p.IPVersion) {
				fail("ip-address-version", codeInvalidIPAddresses,
					"%s address %s does not match ipVersion %s", end.name, it.Value, p.IPVersion)
			}
		}
	}
	return errs
}

// onlyReturnStates reports whether states is exactly {ESTABLISHED, RELATED}.
// A live console refuses allowReturnTraffic with that set, and accepts it
// with either state alone or with NEW or INVALID added.
func onlyReturnStates(states []string) bool {
	got := sortedCopy(states)
	return len(got) == 2 && got[0] == "ESTABLISHED" && got[1] == "RELATED"
}

// ipMatchesVersion reports whether an address or subnet belongs to the IP
// family ipVersion names. IPV4_AND_IPV6 takes either. A value that parses as
// neither is left to the console.
func ipMatchesVersion(value, ipVersion string) bool {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		prefix, perr := netip.ParsePrefix(value)
		if perr != nil {
			return true
		}
		addr = prefix.Addr()
	}
	switch ipVersion {
	case "IPV4":
		return addr.Is4()
	case "IPV6":
		return addr.Is6() && !addr.Is4In6()
	}
	return true
}

package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// exportSite writes the live site as a #Site-shaped document. Passphrases are
// replaced by the name of the environment variable the operator should set.
//
// Objects the schema cannot express are left out and reported on warn rather
// than emitted in a mangled form: the contract of `export` is that its output
// feeds straight back into `diff` as a no-op, and a lossy entry would instead
// become a destructive `PUT`.
func exportSite(c *client, siteID string, out, warn io.Writer) error {
	var doc site

	nets, err := c.networks(siteID)
	if err != nil {
		return err
	}
	netNames := nameLookup{}
	for _, a := range nets {
		netNames[a.ID] = a.Spec.Name
		doc.Networks = append(doc.Networks, a.Spec)
	}

	zones, zonesAvailable, err := c.zones(siteID)
	if err != nil {
		return err
	}
	zoneNames := nameLookup{}
	for _, a := range zones {
		zoneNames[a.ID] = a.Spec.Name
		names, ok := zoneNetworkNames(a.Spec, netNames)
		if !ok {
			fmt.Fprintf(warn, "skipping firewall zone %q: it has member networks that GET /networks "+
				"does not return (WAN interfaces), so they cannot be named. Policies may still "+
				"reference the zone by name.\n", a.Spec.Name)
			continue
		}
		doc.FirewallZones = append(doc.FirewallZones, firewallZone{Name: a.Spec.Name, Networks: names})
	}

	wifis, err := c.wifis(siteID)
	if err != nil {
		return err
	}
	for _, a := range wifis {
		w := a.Spec
		network := "NATIVE"
		if w.Network.Type != "NATIVE" {
			network = netNames.name(w.Network.NetworkID)
		}
		doc.WiFi = append(doc.WiFi, wifi{
			Name:    w.Name,
			Enabled: w.Enabled,
			Network: network,
			Security: wifiSecurity{
				Type: w.SecurityConfiguration.Type,
				// Never export the passphrase itself.
				PassphraseEnv:      passphraseEnvFor(w.Name, w.SecurityConfiguration.Type),
				FastRoamingEnabled: w.SecurityConfiguration.FastRoamingEnabled,
				PMFMode:            w.SecurityConfiguration.PMFMode,
			},
			Bands:                               w.BroadcastingFrequenciesGHz,
			ClientIsolationEnabled:              w.ClientIsolationEnabled,
			HideName:                            w.HideName,
			MulticastToUnicastConversionEnabled: w.MulticastToUnicastConversionEnabled,
			UAPSDEnabled:                        w.UAPSDEnabled,
			BandSteeringEnabled:                 w.BandSteeringEnabled,
			ARPProxyEnabled:                     w.ARPProxyEnabled,
			BSSTransitionEnabled:                w.BSSTransitionEnabled,
			AdvertiseDeviceName:                 w.AdvertiseDeviceName,
		})
	}

	if zonesAvailable {
		policies, available, err := c.firewallPolicies(siteID)
		if err != nil {
			return err
		}
		if available {
			doc.FirewallPolicies = exportPolicies(policies, zoneNames, netNames, warn)
		}
	}

	dns, err := c.dnsPolicies(siteID)
	if err != nil {
		return err
	}
	for _, a := range dns {
		doc.DNSPolicies = append(doc.DNSPolicies, a.Spec)
	}

	// Emit empty lists rather than null so the document round-trips into #Site.
	if doc.Networks == nil {
		doc.Networks = []network{}
	}
	if doc.FirewallZones == nil {
		doc.FirewallZones = []firewallZone{}
	}
	if doc.WiFi == nil {
		doc.WiFi = []wifi{}
	}
	if doc.FirewallPolicies == nil {
		doc.FirewallPolicies = []firewallPolicy{}
	}
	if doc.DNSPolicies == nil {
		doc.DNSPolicies = []dnsPolicy{}
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// exportPolicies renders the live policies. `order` is emitted only for
// USER_DEFINED policies — those are the only ones the console lets anyone
// reorder — and counts from 10 within each zone pair, which is the bucket the
// ordering endpoint works on.
func exportPolicies(policies []actual[apiFirewallPolicy], zoneNames, netNames nameLookup, warn io.Writer) []firewallPolicy {
	var out []firewallPolicy
	nextOrder := map[string]int{}
	for _, a := range policies {
		p := a.Spec.spec(zoneNames, netNames)
		if lossy := policyUnmodelledFields(a, zoneNames, netNames); len(lossy) > 0 {
			fmt.Fprintf(warn, "skipping firewall policy %q: schema/unifi.cue does not model %v. "+
				"Declaring it would plan a PUT that drops those fields.\n", p.key(), lossy)
			continue
		}
		if a.Origin != originSystem {
			nextOrder[p.zonePair()] += 10
			order := nextOrder[p.zonePair()]
			p.Order = &order
		}
		out = append(out, p)
	}
	return out
}

// passphraseEnvFor invents a stable environment-variable name for an exported
// SSID so the operator knows what to set; OPEN networks get none.
func passphraseEnvFor(ssid, securityType string) string {
	if securityType == "OPEN" {
		return ""
	}
	out := make([]rune, 0, len(ssid))
	for _, r := range ssid {
		switch {
		case r >= 'a' && r <= 'z':
			out = append(out, r-32)
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return fmt.Sprintf("UNIFI_WIFI_%s", string(out))
}

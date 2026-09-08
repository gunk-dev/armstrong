package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// exportSite writes the live site as a #Site-shaped document. Passphrases are
// replaced by the name of the environment variable the operator should set.
func exportSite(c *client, siteID string, out io.Writer) error {
	var doc site

	nets, err := c.networks(siteID)
	if err != nil {
		return err
	}
	nameByID := map[string]string{}
	for _, a := range nets {
		nameByID[a.ID] = a.Spec.Name
		doc.Networks = append(doc.Networks, a.Spec)
	}

	zones, zonesAvailable, err := c.zones(siteID)
	if err != nil {
		return err
	}
	zoneNameByID := map[string]string{}
	for _, a := range zones {
		zoneNameByID[a.ID] = a.Spec.Name
		names := make([]string, 0, len(a.Spec.NetworkIDs))
		for _, id := range a.Spec.NetworkIDs {
			names = append(names, nameByID[id])
		}
		sort.Strings(names)
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
			network = nameByID[w.Network.NetworkID]
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
			for i, a := range policies {
				p := a.Spec
				protocol := ""
				if f := p.IPProtocolScope.ProtocolFilter; f != nil {
					protocol = f.Name
				}
				doc.FirewallPolicies = append(doc.FirewallPolicies, firewallPolicy{
					Name:               p.Name,
					Description:        p.Description,
					Enabled:            p.Enabled,
					Action:             p.Action.Type,
					AllowReturnTraffic: p.Action.AllowReturnTraffic,
					SourceZone:         zoneNameByID[p.Source.ZoneID],
					DestinationZone:    zoneNameByID[p.Destination.ZoneID],
					IPVersion:          p.IPProtocolScope.IPVersion,
					Protocol:           protocol,
					ConnectionStates:   p.ConnectionStateFilter,
					LoggingEnabled:     p.LoggingEnabled,
					Order:              (i + 1) * 10,
				})
			}
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

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Origins the API reports. SYSTEM_DEFINED objects are created by the console
// itself: their configurable fields may be updated, but they are never deleted.
const (
	originSystem = "SYSTEM_DEFINED"
	originUser   = "USER_DEFINED"
)

// codeZBFNotConfigured is the error code a console still running the legacy
// firewall returns from every /firewall/zones and /firewall/policies request.
const codeZBFNotConfigured = "api.firewall.zone-based-firewall-not-configured"

type metadata struct {
	Origin       string `json:"origin"`
	Configurable *bool  `json:"configurable,omitempty"`
}

// actual pairs a server object's identity with the desired-shaped projection
// used for comparison.
type actual[T any] struct {
	ID       string
	Origin   string
	Spec     T
	IsSystem bool
}

// ---------------------------------------------------------------- networks

type apiNetwork struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	Management            string   `json:"management"`
	Enabled               bool     `json:"enabled"`
	VlanID                int      `json:"vlanId"`
	Metadata              metadata `json:"metadata"`
	IsolationEnabled      bool     `json:"isolationEnabled"`
	InternetAccessEnabled bool     `json:"internetAccessEnabled"`
	CellularBackupEnabled bool     `json:"cellularBackupEnabled"`
	MDNSForwardingEnabled bool     `json:"mdnsForwardingEnabled"`
	IPv4Configuration     *apiIPv4 `json:"ipv4Configuration,omitempty"`
	Default               bool     `json:"default"`
}

type apiIPv4 struct {
	AutoScaleEnabled  bool         `json:"autoScaleEnabled"`
	HostIPAddress     string       `json:"hostIpAddress"`
	PrefixLength      int          `json:"prefixLength"`
	DHCPConfiguration *apiIPv4DHCP `json:"dhcpConfiguration,omitempty"`
}

type apiIPv4DHCP struct {
	Mode                         string    `json:"mode"`
	IPAddressRange               *apiRange `json:"ipAddressRange,omitempty"`
	DNSServerIPAddressesOverride []string  `json:"dnsServerIpAddressesOverride,omitempty"`
	LeaseTimeSeconds             int       `json:"leaseTimeSeconds,omitempty"`
	DomainName                   string    `json:"domainName,omitempty"`
	PingConflictDetectionEnabled bool      `json:"pingConflictDetectionEnabled,omitempty"`
}

type apiRange struct {
	Start string `json:"start"`
	Stop  string `json:"stop"`
}

// spec projects the API object onto the fields the schema models, so that
// comparison ignores server-side fields we never write.
func (n apiNetwork) spec() network {
	out := network{
		Name:                  n.Name,
		Management:            n.Management,
		Enabled:               n.Enabled,
		VlanID:                n.VlanID,
		IsolationEnabled:      n.IsolationEnabled,
		InternetAccessEnabled: n.InternetAccessEnabled,
		CellularBackupEnabled: n.CellularBackupEnabled,
		MDNSForwardingEnabled: n.MDNSForwardingEnabled,
	}
	if n.IPv4Configuration != nil {
		v4 := &networkIPv4{
			HostIPAddress:    n.IPv4Configuration.HostIPAddress,
			PrefixLength:     n.IPv4Configuration.PrefixLength,
			AutoScaleEnabled: n.IPv4Configuration.AutoScaleEnabled,
		}
		if d := n.IPv4Configuration.DHCPConfiguration; d != nil {
			dh := &networkDHCP{
				Mode:                         d.Mode,
				LeaseTimeSeconds:             d.LeaseTimeSeconds,
				DNSServers:                   d.DNSServerIPAddressesOverride,
				DomainName:                   d.DomainName,
				PingConflictDetectionEnabled: d.PingConflictDetectionEnabled,
			}
			if d.IPAddressRange != nil {
				dh.RangeStart = d.IPAddressRange.Start
				dh.RangeStop = d.IPAddressRange.Stop
			}
			v4.DHCP = dh
		}
		out.IPv4 = v4
	}
	return out
}

// body renders desired state as an Integration API create/update payload.
func (n network) body() map[string]any {
	b := map[string]any{
		"management":            "GATEWAY",
		"name":                  n.Name,
		"enabled":               n.Enabled,
		"vlanId":                n.VlanID,
		"isolationEnabled":      n.IsolationEnabled,
		"internetAccessEnabled": n.InternetAccessEnabled,
		"cellularBackupEnabled": n.CellularBackupEnabled,
		"mdnsForwardingEnabled": n.MDNSForwardingEnabled,
	}
	if n.IPv4 == nil {
		return b
	}
	v4 := map[string]any{
		"autoScaleEnabled": n.IPv4.AutoScaleEnabled,
		"hostIpAddress":    n.IPv4.HostIPAddress,
		"prefixLength":     n.IPv4.PrefixLength,
	}
	if d := n.IPv4.DHCP; d != nil {
		dh := map[string]any{"mode": d.Mode}
		if d.Mode == "SERVER" {
			dh["ipAddressRange"] = map[string]any{"start": d.RangeStart, "stop": d.RangeStop}
			dh["leaseTimeSeconds"] = d.LeaseTimeSeconds
			dh["pingConflictDetectionEnabled"] = d.PingConflictDetectionEnabled
			if len(d.DNSServers) > 0 {
				dh["dnsServerIpAddressesOverride"] = d.DNSServers
			}
			if d.DomainName != "" {
				dh["domainName"] = d.DomainName
			}
		}
		v4["dhcpConfiguration"] = dh
	}
	b["ipv4Configuration"] = v4
	return b
}

func (c *client) networks(siteID string) ([]actual[network], error) {
	raws, err := c.list("/sites/" + siteID + "/networks")
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	out := make([]actual[network], 0, len(raws))
	for _, raw := range raws {
		var overview apiNetwork
		if err := json.Unmarshal(raw, &overview); err != nil {
			return nil, fmt.Errorf("parse network: %w", err)
		}
		// The list response omits ipv4Configuration; fetch the detail view.
		var detail apiNetwork
		if err := c.do(http.MethodGet, "/sites/"+siteID+"/networks/"+overview.ID, nil, &detail); err != nil {
			return nil, fmt.Errorf("get network %q: %w", overview.Name, err)
		}
		out = append(out, actual[network]{
			ID:       detail.ID,
			Origin:   detail.Metadata.Origin,
			Spec:     detail.spec(),
			IsSystem: detail.Metadata.Origin == originSystem,
		})
	}
	return out, nil
}

// ----------------------------------------------------------- firewall zones

type apiZone struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	NetworkIDs []string `json:"networkIds"`
	Metadata   metadata `json:"metadata"`
}

// zones lists the firewall zones. The bool reports whether the zone-based
// firewall is configured at all: a console still running the legacy firewall
// answers 400 with codeZBFNotConfigured rather than returning an empty list.
// Any other failure is a real error — an expired key must never look like an
// unconfigured firewall.
func (c *client) zones(siteID string) ([]actual[apiZone], bool, error) {
	raws, err := c.list("/sites/" + siteID + "/firewall/zones")
	if hasErrorCode(err, codeZBFNotConfigured) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("list firewall zones: %w", err)
	}
	out := make([]actual[apiZone], 0, len(raws))
	for _, raw := range raws {
		var z apiZone
		if err := json.Unmarshal(raw, &z); err != nil {
			return nil, false, fmt.Errorf("parse firewall zone: %w", err)
		}
		out = append(out, actual[apiZone]{ID: z.ID, Origin: z.Metadata.Origin, Spec: z, IsSystem: z.Metadata.Origin == originSystem})
	}
	return out, true, nil
}

// ------------------------------------------------------------------- wifi

type apiWiFi struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Metadata metadata `json:"metadata"`
	Network  struct {
		Type      string `json:"type"`
		NetworkID string `json:"networkId"`
	} `json:"network"`
	SecurityConfiguration struct {
		Type               string `json:"type"`
		Passphrase         string `json:"passphrase"`
		FastRoamingEnabled *bool  `json:"fastRoamingEnabled,omitempty"`
		PMFMode            string `json:"pmfMode,omitempty"`
	} `json:"securityConfiguration"`
	BroadcastingFrequenciesGHz          []float64 `json:"broadcastingFrequenciesGHz"`
	ClientIsolationEnabled              bool      `json:"clientIsolationEnabled"`
	HideName                            bool      `json:"hideName"`
	MulticastToUnicastConversionEnabled bool      `json:"multicastToUnicastConversionEnabled"`
	UAPSDEnabled                        bool      `json:"uapsdEnabled"`
	BandSteeringEnabled                 *bool     `json:"bandSteeringEnabled,omitempty"`
	ARPProxyEnabled                     *bool     `json:"arpProxyEnabled,omitempty"`
	BSSTransitionEnabled                *bool     `json:"bssTransitionEnabled,omitempty"`
	AdvertiseDeviceName                 *bool     `json:"advertiseDeviceName,omitempty"`
}

func (c *client) wifis(siteID string) ([]actual[apiWiFi], error) {
	raws, err := c.list("/sites/" + siteID + "/wifi/broadcasts")
	if err != nil {
		return nil, fmt.Errorf("list wifi broadcasts: %w", err)
	}
	out := make([]actual[apiWiFi], 0, len(raws))
	for _, raw := range raws {
		var overview apiWiFi
		if err := json.Unmarshal(raw, &overview); err != nil {
			return nil, fmt.Errorf("parse wifi broadcast: %w", err)
		}
		var detail apiWiFi
		if err := c.do(http.MethodGet, "/sites/"+siteID+"/wifi/broadcasts/"+overview.ID, nil, &detail); err != nil {
			return nil, fmt.Errorf("get wifi broadcast %q: %w", overview.Name, err)
		}
		// The API returns the passphrase in plaintext; make sure it can never
		// surface in output, however it is quoted.
		registerSecret(detail.SecurityConfiguration.Passphrase)
		out = append(out, actual[apiWiFi]{ID: detail.ID, Origin: detail.Metadata.Origin, Spec: detail, IsSystem: detail.Metadata.Origin == originSystem})
	}
	return out, nil
}

// body renders a STANDARD wifi broadcast payload. passphrase is resolved by
// the caller from the environment and is never logged.
func (w wifi) body(networkID, passphrase string) map[string]any {
	sec := map[string]any{"type": w.Security.Type}
	if w.Security.Type != "OPEN" {
		sec["passphrase"] = passphrase
	}
	if w.Security.FastRoamingEnabled != nil {
		sec["fastRoamingEnabled"] = *w.Security.FastRoamingEnabled
	}
	if w.Security.PMFMode != "" {
		sec["pmfMode"] = w.Security.PMFMode
	}

	net := map[string]any{"type": "NATIVE"}
	if w.Network != "" && w.Network != "NATIVE" {
		net = map[string]any{"type": "SPECIFIC", "networkId": networkID}
	}

	b := map[string]any{
		"type":                                "STANDARD",
		"name":                                w.Name,
		"enabled":                             w.Enabled,
		"network":                             net,
		"securityConfiguration":               sec,
		"broadcastingFrequenciesGHz":          w.Bands,
		"clientIsolationEnabled":              w.ClientIsolationEnabled,
		"hideName":                            w.HideName,
		"multicastToUnicastConversionEnabled": w.MulticastToUnicastConversionEnabled,
		"uapsdEnabled":                        w.UAPSDEnabled,
	}
	for k, v := range map[string]*bool{
		"bandSteeringEnabled":  w.BandSteeringEnabled,
		"arpProxyEnabled":      w.ARPProxyEnabled,
		"bssTransitionEnabled": w.BSSTransitionEnabled,
		"advertiseDeviceName":  w.AdvertiseDeviceName,
	} {
		if v != nil {
			b[k] = *v
		}
	}
	return b
}

// -------------------------------------------------------------- dns policies

type apiDNSPolicy struct {
	ID       string   `json:"id"`
	Metadata metadata `json:"metadata"`
	dnsPolicy
}

func (c *client) dnsPolicies(siteID string) ([]actual[dnsPolicy], error) {
	raws, err := c.list("/sites/" + siteID + "/dns/policies")
	if err != nil {
		return nil, fmt.Errorf("list dns policies: %w", err)
	}
	out := make([]actual[dnsPolicy], 0, len(raws))
	for _, raw := range raws {
		var p apiDNSPolicy
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("parse dns policy: %w", err)
		}
		out = append(out, actual[dnsPolicy]{ID: p.ID, Origin: p.Metadata.Origin, Spec: p.dnsPolicy, IsSystem: p.Metadata.Origin == originSystem})
	}
	return out, nil
}

func (d dnsPolicy) body() map[string]any {
	b := map[string]any{"type": d.Type, "enabled": d.Enabled, "domain": d.Domain}
	set := func(k, v string) {
		if v != "" {
			b[k] = v
		}
	}
	set("ipv4Address", d.IPv4Address)
	set("ipv6Address", d.IPv6Address)
	set("targetDomain", d.TargetDomain)
	set("mailServerDomain", d.MailServerDomain)
	set("text", d.Text)
	set("serverDomain", d.ServerDomain)
	set("service", d.Service)
	set("protocol", d.Protocol)
	set("ipAddress", d.IPAddress)
	for k, v := range map[string]*int{"port": d.Port, "priority": d.Priority, "weight": d.Weight, "ttlSeconds": d.TTLSeconds} {
		if v != nil {
			b[k] = *v
		}
	}
	return b
}

// --------------------------------------------------------- firewall policies

type apiFirewallPolicy struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Enabled     bool     `json:"enabled"`
	Metadata    metadata `json:"metadata"`
	Action      struct {
		Type               string `json:"type"`
		AllowReturnTraffic bool   `json:"allowReturnTraffic"`
	} `json:"action"`
	Source          apiPolicyEndpoint `json:"source"`
	Destination     apiPolicyEndpoint `json:"destination"`
	IPProtocolScope struct {
		IPVersion      string `json:"ipVersion"`
		ProtocolFilter *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"protocolFilter,omitempty"`
	} `json:"ipProtocolScope"`
	ConnectionStateFilter []string `json:"connectionStateFilter,omitempty"`
	LoggingEnabled        bool     `json:"loggingEnabled"`
}

type apiPolicyEndpoint struct {
	ZoneID        string `json:"zoneId"`
	TrafficFilter *struct {
		Type          string `json:"type"`
		NetworkFilter *struct {
			NetworkIDs    []string `json:"networkIds"`
			MatchOpposite bool     `json:"matchOpposite"`
		} `json:"networkFilter,omitempty"`
		PortFilter *struct {
			Type          string `json:"type"`
			MatchOpposite bool   `json:"matchOpposite"`
			Items         []struct {
				Type      string `json:"type"`
				Port      int    `json:"port,omitempty"`
				StartPort int    `json:"startPort,omitempty"`
				EndPort   int    `json:"endPort,omitempty"`
			} `json:"items,omitempty"`
		} `json:"portFilter,omitempty"`
	} `json:"trafficFilter,omitempty"`
}

// firewallPolicies lists the zone-based firewall policies. As with zones, the
// bool distinguishes "feature not configured" from a genuine failure.
func (c *client) firewallPolicies(siteID string) ([]actual[apiFirewallPolicy], bool, error) {
	raws, err := c.list("/sites/" + siteID + "/firewall/policies")
	if hasErrorCode(err, codeZBFNotConfigured) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("list firewall policies: %w", err)
	}
	out := make([]actual[apiFirewallPolicy], 0, len(raws))
	for _, raw := range raws {
		var p apiFirewallPolicy
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, false, fmt.Errorf("parse firewall policy: %w", err)
		}
		out = append(out, actual[apiFirewallPolicy]{ID: p.ID, Origin: p.Metadata.Origin, Spec: p, IsSystem: p.Metadata.Origin == originSystem})
	}
	return out, true, nil
}

// body renders a firewall policy payload. zones and networks have already been
// resolved to ids by the caller.
func (p firewallPolicy) body(srcZoneID, dstZoneID string, srcNetIDs, dstNetIDs []string) map[string]any {
	action := map[string]any{"type": p.Action}
	if p.Action == "ALLOW" {
		action["allowReturnTraffic"] = p.AllowReturnTraffic
	}

	scope := map[string]any{"ipVersion": p.IPVersion}
	if p.Protocol != "" {
		scope["protocolFilter"] = map[string]any{"type": "NAMED", "name": p.Protocol}
	}

	b := map[string]any{
		"name":            p.Name,
		"enabled":         p.Enabled,
		"action":          action,
		"source":          policyEndpoint(srcZoneID, srcNetIDs, p.SourcePorts),
		"destination":     policyEndpoint(dstZoneID, dstNetIDs, p.DestinationPorts),
		"ipProtocolScope": scope,
		"loggingEnabled":  p.LoggingEnabled,
	}
	if p.Description != "" {
		b["description"] = p.Description
	}
	if len(p.ConnectionStates) > 0 {
		b["connectionStateFilter"] = p.ConnectionStates
	}
	return b
}

func policyEndpoint(zoneID string, networkIDs, ports []string) map[string]any {
	ep := map[string]any{"zoneId": zoneID}
	if len(networkIDs) == 0 && len(ports) == 0 {
		return ep
	}
	tf := map[string]any{"type": "NETWORK"}
	if len(networkIDs) > 0 {
		sorted := append([]string(nil), networkIDs...)
		sort.Strings(sorted)
		tf["networkFilter"] = map[string]any{"networkIds": sorted, "matchOpposite": false}
	}
	if len(ports) > 0 {
		tf["type"] = "PORT"
		tf["portFilter"] = map[string]any{"type": "PORTS", "matchOpposite": false, "items": portItems(ports)}
	}
	ep["trafficFilter"] = tf
	return ep
}

func portItems(ports []string) []map[string]any {
	items := make([]map[string]any, 0, len(ports))
	for _, p := range ports {
		if start, end, ok := splitPortRange(p); ok {
			items = append(items, map[string]any{"type": "PORT_NUMBER_RANGE", "startPort": start, "endPort": end})
			continue
		}
		items = append(items, map[string]any{"type": "PORT_NUMBER", "port": atoiOrZero(p)})
	}
	return items
}

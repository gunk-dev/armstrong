package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
	// Configurable is the console's own verdict on whether the object may be
	// updated. Absent on object kinds that do not report it, which the API
	// treats as "yes".
	Configurable bool
	// Raw is the object exactly as the console returned it, kept so that a
	// projection can be checked for information it silently dropped.
	Raw json.RawMessage
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
		out = append(out, actual[apiZone]{
			ID:           z.ID,
			Origin:       z.Metadata.Origin,
			Spec:         z,
			IsSystem:     z.Metadata.Origin == originSystem,
			Configurable: z.Metadata.Configurable == nil || *z.Metadata.Configurable,
			Raw:          raw,
		})
	}
	return out, true, nil
}

// zoneNetworkNames turns a zone's member ids into the names an instance file
// would use. ok is false when a member is not in `nets` — which happens for
// real: the console's `External` zone lists WAN interfaces, and `GET /networks`
// does not return those. Such a zone cannot be expressed as a #FirewallZone,
// and writing one back would empty it.
func zoneNetworkNames(z apiZone, nets nameLookup) (names []string, ok bool) {
	names = make([]string, 0, len(z.NetworkIDs))
	for _, id := range z.NetworkIDs {
		name := nets.name(id)
		if name == "" {
			return nil, false
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true
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

// apiFirewallPolicy is the console's representation of a zone-based firewall
// policy, as confirmed against a live 10.6 console (see
// docs/unifi-api-notes.md). Every field the console returns is modelled here;
// the round-trip check in policyUnmodelledFields is what proves it.
type apiFirewallPolicy struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Enabled     bool     `json:"enabled"`
	Index       int      `json:"index"`
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
			Type     string `json:"type"`
			Protocol struct {
				Name string `json:"name"`
			} `json:"protocol"`
			MatchOpposite bool `json:"matchOpposite"`
		} `json:"protocolFilter,omitempty"`
	} `json:"ipProtocolScope"`
	ConnectionStateFilter []string     `json:"connectionStateFilter,omitempty"`
	LoggingEnabled        bool         `json:"loggingEnabled"`
	Schedule              *apiSchedule `json:"schedule,omitempty"`
}

type apiPolicyEndpoint struct {
	ZoneID        string            `json:"zoneId"`
	TrafficFilter *apiTrafficFilter `json:"trafficFilter,omitempty"`
}

type apiTrafficFilter struct {
	Type          string `json:"type"`
	NetworkFilter *struct {
		NetworkIDs    []string `json:"networkIds"`
		MatchOpposite bool     `json:"matchOpposite"`
	} `json:"networkFilter,omitempty"`
	IPAddressFilter *struct {
		Type          string `json:"type"`
		MatchOpposite bool   `json:"matchOpposite"`
		Items         []struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"items"`
	} `json:"ipAddressFilter,omitempty"`
	PortFilter *struct {
		Type          string `json:"type"`
		MatchOpposite bool   `json:"matchOpposite"`
		Items         []struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"items"`
	} `json:"portFilter,omitempty"`
	MACAddressFilter *struct {
		MACAddresses []string `json:"macAddresses"`
	} `json:"macAddressFilter,omitempty"`
	ApplicationFilter *struct {
		ApplicationIDs []int `json:"applicationIds"`
	} `json:"applicationFilter,omitempty"`
}

type apiSchedule struct {
	Mode       string `json:"mode"`
	TimeFilter *struct {
		StartTime string `json:"startTime"`
		StopTime  string `json:"stopTime"`
	} `json:"timeFilter,omitempty"`
	RepeatOnDays []string `json:"repeatOnDays,omitempty"`
	StartDate    string   `json:"startDate,omitempty"`
	StopDate     string   `json:"stopDate,omitempty"`
}

// The inner filter type constants the API fills in for itself.
const (
	filterTypeIPAddresses = "IP_ADDRESSES"
	filterTypePorts       = "PORTS"
	filterTypeNamedProto  = "NAMED_PROTOCOL"
)

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
		out = append(out, actual[apiFirewallPolicy]{
			ID:           p.ID,
			Origin:       p.Metadata.Origin,
			Spec:         p,
			IsSystem:     p.Metadata.Origin == originSystem,
			Configurable: p.Metadata.Configurable == nil || *p.Metadata.Configurable,
			Raw:          raw,
		})
	}
	return out, true, nil
}

// nameLookup turns a server id into the name an instance file would use. A
// miss yields "", which is never a legal name — so a projection that hits one
// fails the round-trip check rather than being written back.
type nameLookup map[string]string

func (l nameLookup) name(id string) string { return l[id] }

// spec projects a live policy onto the fields schema/unifi.cue models. zones
// and nets map server ids back to names.
func (p apiFirewallPolicy) spec(zones, nets nameLookup) firewallPolicy {
	out := firewallPolicy{
		Name:               p.Name,
		Description:        p.Description,
		Enabled:            p.Enabled,
		Action:             p.Action.Type,
		AllowReturnTraffic: p.Action.AllowReturnTraffic,
		SourceZone:         zones.name(p.Source.ZoneID),
		DestinationZone:    zones.name(p.Destination.ZoneID),
		Source:             p.Source.TrafficFilter.spec(nets),
		Destination:        p.Destination.TrafficFilter.spec(nets),
		IPVersion:          p.IPProtocolScope.IPVersion,
		ConnectionStates:   p.ConnectionStateFilter,
		LoggingEnabled:     p.LoggingEnabled,
	}
	if f := p.IPProtocolScope.ProtocolFilter; f != nil {
		out.Protocol = f.Protocol.Name
		out.ProtocolMatchOpposite = f.MatchOpposite
	}
	if s := p.Schedule; s != nil {
		sched := &firewallSchedule{
			Mode:         s.Mode,
			RepeatOnDays: s.RepeatOnDays,
			StartDate:    s.StartDate,
			StopDate:     s.StopDate,
		}
		if s.TimeFilter != nil {
			sched.StartTime = s.TimeFilter.StartTime
			sched.StopTime = s.TimeFilter.StopTime
		}
		out.Schedule = sched
	}
	return out
}

func (f *apiTrafficFilter) spec(nets nameLookup) *trafficFilter {
	if f == nil {
		return nil
	}
	out := &trafficFilter{Type: f.Type}
	if n := f.NetworkFilter; n != nil {
		names := make([]string, 0, len(n.NetworkIDs))
		for _, id := range n.NetworkIDs {
			names = append(names, nets.name(id))
		}
		out.NetworkFilter = &networkFilter{Networks: names, MatchOpposite: n.MatchOpposite}
	}
	if a := f.IPAddressFilter; a != nil {
		items := make([]ipAddressMatch, 0, len(a.Items))
		for _, it := range a.Items {
			items = append(items, ipAddressMatch{Type: it.Type, Value: it.Value})
		}
		out.IPAddressFilter = &ipAddressFilter{Items: items, MatchOpposite: a.MatchOpposite}
	}
	if pf := f.PortFilter; pf != nil {
		items := make([]string, 0, len(pf.Items))
		for _, it := range pf.Items {
			items = append(items, portItemString(it.Type, it.Value))
		}
		out.PortFilter = &portFilter{Items: items, MatchOpposite: pf.MatchOpposite}
	}
	if m := f.MACAddressFilter; m != nil {
		out.MACAddressFilter = &macAddressFilter{MACAddresses: m.MACAddresses}
	}
	if ap := f.ApplicationFilter; ap != nil {
		out.ApplicationFilter = &applicationFilter{ApplicationIDs: ap.ApplicationIDs}
	}
	return out
}

// portItemString renders one live port item as the #Port string form. An item
// shape the schema does not model yields "", which fails the round-trip check.
func portItemString(itemType string, value json.RawMessage) string {
	var n int
	if itemType == "PORT_NUMBER" && json.Unmarshal(value, &n) == nil {
		return strconv.Itoa(n)
	}
	var s string
	if itemType == "PORT_NUMBER_RANGE" && json.Unmarshal(value, &s) == nil {
		if start, end, ok := splitPortRange(s); ok && validPort(start) && validPort(end) {
			return s
		}
	}
	return ""
}

// body renders a firewall policy payload. resolveZone and resolveNetwork turn
// the names in the instance file into the ids the API wants. It fails on a
// malformed port rather than sending a policy that silently matches something
// else.
func (p firewallPolicy) body(resolveZone, resolveNetwork func(string) (string, error)) (map[string]any, error) {
	srcZone, err := resolveZone(p.SourceZone)
	if err != nil {
		return nil, fmt.Errorf("source zone: %w", err)
	}
	dstZone, err := resolveZone(p.DestinationZone)
	if err != nil {
		return nil, fmt.Errorf("destination zone: %w", err)
	}

	action := map[string]any{"type": p.Action}
	if p.Action == "ALLOW" {
		action["allowReturnTraffic"] = p.AllowReturnTraffic
	}

	scope := map[string]any{"ipVersion": p.IPVersion}
	if p.Protocol != "" {
		scope["protocolFilter"] = map[string]any{
			"type":          filterTypeNamedProto,
			"protocol":      map[string]any{"name": p.Protocol},
			"matchOpposite": p.ProtocolMatchOpposite,
		}
	}

	source, err := policyEndpoint(srcZone, p.Source, resolveNetwork)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	destination, err := policyEndpoint(dstZone, p.Destination, resolveNetwork)
	if err != nil {
		return nil, fmt.Errorf("destination: %w", err)
	}

	b := map[string]any{
		"name":            p.Name,
		"enabled":         p.Enabled,
		"action":          action,
		"source":          source,
		"destination":     destination,
		"ipProtocolScope": scope,
		"loggingEnabled":  p.LoggingEnabled,
	}
	if p.Description != "" {
		b["description"] = p.Description
	}
	if len(p.ConnectionStates) > 0 {
		b["connectionStateFilter"] = p.ConnectionStates
	}
	if s := p.Schedule; s != nil {
		sched := map[string]any{"mode": s.Mode}
		if s.StartTime != "" || s.StopTime != "" {
			sched["timeFilter"] = map[string]any{"startTime": s.StartTime, "stopTime": s.StopTime}
		}
		if len(s.RepeatOnDays) > 0 {
			sched["repeatOnDays"] = s.RepeatOnDays
		}
		if s.StartDate != "" {
			sched["startDate"] = s.StartDate
		}
		if s.StopDate != "" {
			sched["stopDate"] = s.StopDate
		}
		b["schedule"] = sched
	}
	return b, nil
}

func policyEndpoint(zoneID string, f *trafficFilter, resolveNetwork func(string) (string, error)) (map[string]any, error) {
	ep := map[string]any{"zoneId": zoneID}
	if f == nil {
		return ep, nil
	}
	tf := map[string]any{"type": f.Type}
	if n := f.NetworkFilter; n != nil {
		ids := make([]string, 0, len(n.Networks))
		for _, name := range n.Networks {
			id, err := resolveNetwork(name)
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		tf["networkFilter"] = map[string]any{"networkIds": ids, "matchOpposite": n.MatchOpposite}
	}
	if a := f.IPAddressFilter; a != nil {
		items := make([]map[string]any, 0, len(a.Items))
		for _, it := range a.Items {
			items = append(items, map[string]any{"type": it.Type, "value": it.Value})
		}
		tf["ipAddressFilter"] = map[string]any{
			"type": filterTypeIPAddresses, "matchOpposite": a.MatchOpposite, "items": items,
		}
	}
	if pf := f.PortFilter; pf != nil {
		items, err := portItems(pf.Items)
		if err != nil {
			return nil, err
		}
		tf["portFilter"] = map[string]any{
			"type": filterTypePorts, "matchOpposite": pf.MatchOpposite, "items": items,
		}
	}
	if m := f.MACAddressFilter; m != nil {
		tf["macAddressFilter"] = map[string]any{"macAddresses": m.MACAddresses}
	}
	if ap := f.ApplicationFilter; ap != nil {
		tf["applicationFilter"] = map[string]any{"applicationIds": ap.ApplicationIDs}
	}
	ep["trafficFilter"] = tf
	return ep, nil
}

// policyUnmodelledFields reports the JSON paths on which a live policy and the
// tool's own rendering of it disagree. An empty result means the projection is
// lossless: what the tool would PUT back is byte-for-byte what the console
// already holds, so managing the policy cannot silently drop anything.
//
// This is the check that makes it safe to declare a policy at all. `diff` and
// `sync` refuse to touch a policy with a non-empty result rather than planning
// a PUT that would strip whatever the schema has not caught up with.
func policyUnmodelledFields(a actual[apiFirewallPolicy], zones, nets nameLookup) []string {
	live := map[string]any{}
	if err := json.Unmarshal(a.Raw, &live); err != nil {
		return []string{"<unparseable policy>"}
	}
	// Server-owned keys are never written, so they are not part of the check.
	for _, k := range []string{"id", "metadata", "index"} {
		delete(live, k)
	}

	byName := nameLookup{}
	for id, name := range nets {
		byName[name] = id
	}
	zoneByName := nameLookup{}
	for id, name := range zones {
		zoneByName[name] = id
	}
	// Resolvers that cannot fail: an unknown name becomes a sentinel that
	// cannot match any real id, so the mismatch is reported rather than hidden
	// behind an error.
	resolve := func(m nameLookup) func(string) (string, error) {
		return func(name string) (string, error) {
			if id, ok := m[name]; ok && name != "" {
				return id, nil
			}
			return "<unresolved:" + name + ">", nil
		}
	}

	rendered, err := a.Spec.spec(zones, nets).body(resolve(zoneByName), resolve(byName))
	if err != nil {
		return []string{err.Error()}
	}
	// Round-trip the rendering through JSON so both sides use the same Go
	// types (float64 for every number, []any for every list).
	buf, err := json.Marshal(rendered)
	if err != nil {
		return []string{err.Error()}
	}
	var want map[string]any
	if err := json.Unmarshal(buf, &want); err != nil {
		return []string{err.Error()}
	}

	var paths []string
	jsonDiffPaths("", want, live, &paths)
	sort.Strings(paths)
	return paths
}

// jsonDiffPaths walks two decoded JSON values and appends the dotted paths at
// which they differ.
func jsonDiffPaths(path string, want, got any, out *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*out = append(*out, at(path))
			return
		}
		for k, wv := range w {
			gv, present := g[k]
			if !present {
				*out = append(*out, at(join(path, k)))
				continue
			}
			jsonDiffPaths(join(path, k), wv, gv, out)
		}
		for k := range g {
			if _, present := w[k]; !present {
				*out = append(*out, at(join(path, k)))
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			*out = append(*out, at(path))
			return
		}
		for i := range w {
			jsonDiffPaths(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], out)
		}
	default:
		if want != got {
			*out = append(*out, at(path))
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func at(path string) string {
	if path == "" {
		return "<whole object>"
	}
	return path
}

// portItems renders "443" and "8000-8100" as the two item shapes the API
// distinguishes; both carry the number or range under `value`. A port that
// does not parse is an error: silently sending 0 would produce a policy that
// matches something other than what was written.
func portItems(ports []string) ([]map[string]any, error) {
	items := make([]map[string]any, 0, len(ports))
	for _, p := range ports {
		if start, end, ok := splitPortRange(p); ok {
			if !validPort(start) || !validPort(end) || start > end {
				return nil, fmt.Errorf("port range %q is not a valid 1-65535 range", p)
			}
			items = append(items, map[string]any{"type": "PORT_NUMBER_RANGE", "value": p})
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || !validPort(n) {
			return nil, fmt.Errorf("port %q is not a number in 1-65535", p)
		}
		items = append(items, map[string]any{"type": "PORT_NUMBER", "value": n})
	}
	return items, nil
}

func validPort(n int) bool { return n >= 1 && n <= 65535 }

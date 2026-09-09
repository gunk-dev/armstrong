package main

// Desired state, shaped exactly like `cue export` output of schema.#Site.
// Objects are identified by name; ids are server-assigned and never appear in
// an instance file.

type site struct {
	Networks         []network        `json:"networks"`
	FirewallZones    []firewallZone   `json:"firewallZones"`
	WiFi             []wifi           `json:"wifi"`
	FirewallPolicies []firewallPolicy `json:"firewallPolicies"`
	DNSPolicies      []dnsPolicy      `json:"dnsPolicies"`
}

type network struct {
	Name                  string       `json:"name"`
	Management            string       `json:"management"`
	Enabled               bool         `json:"enabled"`
	VlanID                int          `json:"vlanId"`
	IsolationEnabled      bool         `json:"isolationEnabled"`
	InternetAccessEnabled bool         `json:"internetAccessEnabled"`
	CellularBackupEnabled bool         `json:"cellularBackupEnabled"`
	MDNSForwardingEnabled bool         `json:"mdnsForwardingEnabled"`
	IPv4                  *networkIPv4 `json:"ipv4,omitempty"`
}

type networkIPv4 struct {
	HostIPAddress    string       `json:"hostIpAddress"`
	PrefixLength     int          `json:"prefixLength"`
	AutoScaleEnabled bool         `json:"autoScaleEnabled"`
	DHCP             *networkDHCP `json:"dhcp,omitempty"`
}

type networkDHCP struct {
	Mode                         string   `json:"mode"`
	RangeStart                   string   `json:"rangeStart,omitempty"`
	RangeStop                    string   `json:"rangeStop,omitempty"`
	LeaseTimeSeconds             int      `json:"leaseTimeSeconds,omitempty"`
	DNSServers                   []string `json:"dnsServers,omitempty"`
	DomainName                   string   `json:"domainName,omitempty"`
	PingConflictDetectionEnabled bool     `json:"pingConflictDetectionEnabled,omitempty"`
}

type firewallZone struct {
	Name     string   `json:"name"`
	Networks []string `json:"networks"`
}

type wifi struct {
	Name                                string       `json:"name"`
	Enabled                             bool         `json:"enabled"`
	Network                             string       `json:"network"`
	Security                            wifiSecurity `json:"security"`
	Bands                               []float64    `json:"bands"`
	ClientIsolationEnabled              bool         `json:"clientIsolationEnabled"`
	HideName                            bool         `json:"hideName"`
	MulticastToUnicastConversionEnabled bool         `json:"multicastToUnicastConversionEnabled"`
	UAPSDEnabled                        bool         `json:"uapsdEnabled"`
	BandSteeringEnabled                 *bool        `json:"bandSteeringEnabled,omitempty"`
	ARPProxyEnabled                     *bool        `json:"arpProxyEnabled,omitempty"`
	BSSTransitionEnabled                *bool        `json:"bssTransitionEnabled,omitempty"`
	AdvertiseDeviceName                 *bool        `json:"advertiseDeviceName,omitempty"`
}

type wifiSecurity struct {
	Type string `json:"type"`
	// PassphraseEnv names an environment variable holding the passphrase. The
	// passphrase itself is never part of the instance file, and never printed.
	PassphraseEnv      string `json:"passphraseEnv,omitempty"`
	FastRoamingEnabled *bool  `json:"fastRoamingEnabled,omitempty"`
	PMFMode            string `json:"pmfMode,omitempty"`
}

// firewallPolicy is a zone-based firewall rule. Zones and networks are named,
// never referenced by id.
type firewallPolicy struct {
	Name                  string            `json:"name"`
	Description           string            `json:"description,omitempty"`
	Enabled               bool              `json:"enabled"`
	Action                string            `json:"action"`
	AllowReturnTraffic    bool              `json:"allowReturnTraffic"`
	SourceZone            string            `json:"sourceZone"`
	DestinationZone       string            `json:"destinationZone"`
	Source                *trafficFilter    `json:"source,omitempty"`
	Destination           *trafficFilter    `json:"destination,omitempty"`
	IPVersion             string            `json:"ipVersion"`
	Protocol              string            `json:"protocol,omitempty"`
	ProtocolMatchOpposite bool              `json:"protocolMatchOpposite,omitempty"`
	ConnectionStates      []string          `json:"connectionStates,omitempty"`
	LoggingEnabled        bool              `json:"loggingEnabled"`
	Schedule              *firewallSchedule `json:"schedule,omitempty"`

	// Order is the relative position among the USER_DEFINED policies sharing
	// this policy's zone pair. nil means "leave it where it is".
	Order *int `json:"order,omitempty"`
}

// key is the identity used to match desired against actual. Policy names are
// not unique — a stock console reuses "Allow All Traffic" nineteen times — but
// the zone pair plus the name is, on every console seen so far.
func (p firewallPolicy) key() string {
	return p.SourceZone + " -> " + p.DestinationZone + " / " + p.Name
}

// zonePair identifies the ordering bucket a policy belongs to: the console
// orders policies per source/destination zone pair, not site-wide.
func (p firewallPolicy) zonePair() string { return p.SourceZone + " -> " + p.DestinationZone }

// trafficFilter narrows one end of a policy. Several sub-filters may be set at
// once; Type names the one the console treats as primary.
type trafficFilter struct {
	Type              string             `json:"type"`
	NetworkFilter     *networkFilter     `json:"networkFilter,omitempty"`
	IPAddressFilter   *ipAddressFilter   `json:"ipAddressFilter,omitempty"`
	PortFilter        *portFilter        `json:"portFilter,omitempty"`
	MACAddressFilter  *macAddressFilter  `json:"macAddressFilter,omitempty"`
	ApplicationFilter *applicationFilter `json:"applicationFilter,omitempty"`
}

type networkFilter struct {
	Networks      []string `json:"networks"`
	MatchOpposite bool     `json:"matchOpposite"`
}

type ipAddressFilter struct {
	Items         []ipAddressMatch `json:"items"`
	MatchOpposite bool             `json:"matchOpposite"`
}

type ipAddressMatch struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// portFilter items are the #Port strings: "443" or "8000-8100".
type portFilter struct {
	Items         []string `json:"items"`
	MatchOpposite bool     `json:"matchOpposite"`
}

type macAddressFilter struct {
	MACAddresses []string `json:"macAddresses"`
}

type applicationFilter struct {
	ApplicationIDs []int `json:"applicationIds"`
}

type firewallSchedule struct {
	Mode         string   `json:"mode"`
	StartTime    string   `json:"startTime,omitempty"`
	StopTime     string   `json:"stopTime,omitempty"`
	RepeatOnDays []string `json:"repeatOnDays,omitempty"`
	StartDate    string   `json:"startDate,omitempty"`
	StopDate     string   `json:"stopDate,omitempty"`
}

// dnsPolicy carries the union of every record type the API accepts; which
// fields are populated depends on Type. The CUE schema enforces the valid
// combinations.
type dnsPolicy struct {
	Type             string `json:"type"`
	Enabled          bool   `json:"enabled"`
	Domain           string `json:"domain"`
	IPv4Address      string `json:"ipv4Address,omitempty"`
	IPv6Address      string `json:"ipv6Address,omitempty"`
	TargetDomain     string `json:"targetDomain,omitempty"`
	MailServerDomain string `json:"mailServerDomain,omitempty"`
	Text             string `json:"text,omitempty"`
	ServerDomain     string `json:"serverDomain,omitempty"`
	Service          string `json:"service,omitempty"`
	Protocol         string `json:"protocol,omitempty"`
	IPAddress        string `json:"ipAddress,omitempty"`
	Port             *int   `json:"port,omitempty"`
	Priority         *int   `json:"priority,omitempty"`
	Weight           *int   `json:"weight,omitempty"`
	TTLSeconds       *int   `json:"ttlSeconds,omitempty"`
}

// key is the identity used to match desired against actual. DNS policies have
// no name, so they are keyed by type plus the domain they answer for.
func (d dnsPolicy) key() string { return d.Type + " " + d.Domain }

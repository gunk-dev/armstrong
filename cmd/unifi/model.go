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

type firewallPolicy struct {
	Name                string   `json:"name"`
	Description         string   `json:"description,omitempty"`
	Enabled             bool     `json:"enabled"`
	Action              string   `json:"action"`
	AllowReturnTraffic  bool     `json:"allowReturnTraffic"`
	SourceZone          string   `json:"sourceZone"`
	DestinationZone     string   `json:"destinationZone"`
	IPVersion           string   `json:"ipVersion"`
	Protocol            string   `json:"protocol,omitempty"`
	SourceNetworks      []string `json:"sourceNetworks,omitempty"`
	DestinationNetworks []string `json:"destinationNetworks,omitempty"`
	SourcePorts         []string `json:"sourcePorts,omitempty"`
	DestinationPorts    []string `json:"destinationPorts,omitempty"`
	ConnectionStates    []string `json:"connectionStates,omitempty"`
	LoggingEnabled      bool     `json:"loggingEnabled"`
	Order               int      `json:"order"`
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

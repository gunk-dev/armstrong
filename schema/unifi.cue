package schema

// UniFi Network Integration API config-as-code.
//
// These definitions model the official UniFi Network Integration API
// (https://<console>/proxy/network/integration/v1) as served by UniFi Network
// 10.x. Objects are matched by NAME, never by id: ids are server-assigned and
// must not be committed to a consumer repo.
//
// Fields mirror the API's own names and enum values so that `unifi export`
// output can be pasted straight into an instance file.

// #Site is the top-level document consumed by `unifi diff` / `unifi sync`.
#Site: {
	networks: [...#Network]
	firewallZones: [...#FirewallZone]
	wifi: [...#WiFi]
	firewallPolicies: [...#FirewallPolicy]
	dnsPolicies: [...#DNSPolicy]
}

// #Network is a gateway-managed L3 network (a VLAN with an IPv4 subnet and,
// optionally, a DHCP server). Only GATEWAY management is modelled: switch- and
// unmanaged networks need a device id, which is server-assigned.
#Network: {
	name:       string & !=""
	management: "GATEWAY" | *"GATEWAY"
	enabled:    bool | *true

	// VLAN ID. Must be 1 for the default network and >= 2 for any other.
	vlanId: int & >=1 & <=4009

	// Isolate this network from every other network.
	isolationEnabled: bool | *false
	// Allow clients on this network to reach the internet.
	internetAccessEnabled: bool | *true
	// Allow this network to fail over to cellular when the WAN is down.
	cellularBackupEnabled: bool | *false
	// Forward mDNS between this network and others.
	mdnsForwardingEnabled: bool | *false

	ipv4?: #NetworkIPv4
}

#NetworkIPv4: {
	// Gateway address on this network, e.g. "192.0.2.1".
	hostIpAddress: string & !=""
	prefixLength:  int & >=8 & <=30
	// Let the console pick a free subnet instead of honouring hostIpAddress.
	autoScaleEnabled: bool | *false

	dhcp?: #NetworkDHCP
}

#NetworkDHCP: {
	mode: "SERVER" | "RELAY" | "NONE"

	// Required when mode is "SERVER".
	rangeStart?:       string
	rangeStop?:        string
	leaseTimeSeconds?: int & >=0 & <=31536000
	// DNS servers handed out instead of the gateway's own (at most four).
	dnsServers?: [...string]
	domainName?:                   string
	pingConflictDetectionEnabled?: bool

	if mode == "SERVER" {
		rangeStart!:       string & !=""
		rangeStop!:        string & !=""
		leaseTimeSeconds!: int & >=0 & <=31536000
	}
}

// #FirewallZone groups networks so that policies can be written between zones.
// Member networks are referenced by name.
#FirewallZone: {
	name: string & !=""
	networks: [...string]
}

// #WiFi is a STANDARD WiFi broadcast (SSID).
#WiFi: {
	name:    string & !="" & =~"^.{1,32}$"
	enabled: bool | *true

	// "NATIVE" broadcasts on the AP's native network; otherwise the NAME of a
	// #Network declared in the same #Site.
	network: string | *"NATIVE"

	security: #WiFiSecurity

	// 2.4, 5 and/or 6 GHz.
	bands: [...(2.4 | 5 | 6)] & [_, ...] | *[2.4, 5]

	clientIsolationEnabled: bool | *false
	// Hide the SSID from beacons.
	hideName:                            bool | *false
	multicastToUnicastConversionEnabled: bool | *true
	uapsdEnabled:                        bool | *false
	bandSteeringEnabled?:                bool
	arpProxyEnabled?:                    bool
	bssTransitionEnabled?:               bool
	advertiseDeviceName?:                bool
}

#WiFiSecurity: {
	type: "OPEN" | "WPA2_PERSONAL" | "WPA3_PERSONAL" | "WPA2_WPA3_PERSONAL"

	// Name of an environment variable holding the passphrase. The passphrase
	// itself is never committed; `unifi sync` reads the variable at run time.
	// Required for every type except "OPEN".
	passphraseEnv?: string & !=""

	fastRoamingEnabled?: bool
	pmfMode?:            "REQUIRED" | "OPTIONAL"

	if type != "OPEN" {
		passphraseEnv!: string & !=""
	}
}

// #FirewallPolicy is a zone-based firewall rule. Zones and networks are
// referenced by name.
//
// NOTE: the zone-based firewall endpoints are documented but were not
// exercised against a live console (the reference console still runs the
// legacy firewall, so /firewall/zones returns "Zone Based Firewall is not
// configured"). Treat this section as best-effort until it is confirmed.
#FirewallPolicy: {
	name:        string & !=""
	description?: string
	enabled:     bool | *true

	action: "ALLOW" | "BLOCK" | "REJECT"
	// Only meaningful for "ALLOW": permit the reply traffic of a matched flow.
	allowReturnTraffic: bool | *true

	// Names of #FirewallZone entries.
	sourceZone:      string & !=""
	destinationZone: string & !=""

	ipVersion: "IPV4" | "IPV6" | "IPV4_AND_IPV6" | *"IPV4_AND_IPV6"

	// IP protocol name as the API spells it ("tcp", "udp", "tcp_udp", "icmp",
	// "icmpv6", "gre", "esp", …). Omit to match every protocol.
	protocol?: string & !=""

	// Restrict the policy to specific member networks (by name) of the zone.
	sourceNetworks?: [...string]
	destinationNetworks?: [...string]

	// Port numbers or "start-end" ranges, e.g. "443" or "8000-8100".
	sourcePorts?: [...string]
	destinationPorts?: [...string]

	connectionStates?: [...("NEW" | "INVALID" | "ESTABLISHED" | "RELATED")]
	loggingEnabled: bool | *false

	// Relative position among user-defined policies. Lower runs first.
	// Policies are ordered ahead of the system-defined ones.
	order: int | *100
}

// #DNSPolicy is a local DNS record or forwarding rule served by the gateway.
#DNSPolicy: {
	enabled: bool | *true
	domain:  string & !="" & =~"^.{1,127}$"

	{
		type:        "A_RECORD"
		ipv4Address: string & !=""
		ttlSeconds:  int & >=0 & <=86400 | *0
	} | {
		type:        "AAAA_RECORD"
		ipv6Address: string & !=""
		ttlSeconds:  int & >=0 & <=86400 | *0
	} | {
		type:         "CNAME_RECORD"
		targetDomain: string & !=""
		ttlSeconds:   int & >=0 & <=604800 | *0
	} | {
		type:             "MX_RECORD"
		mailServerDomain: string & !=""
		priority:         int & >=0 & <=65535
	} | {
		type: "TXT_RECORD"
		text: string & !="" & =~"^.{1,1024}$"
	} | {
		type:         "SRV_RECORD"
		serverDomain: string & !=""
		service:      string & !=""
		protocol:     string & !=""
		port:         int & >=0 & <=65535
		priority:     int & >=0 & <=65535
		weight:       int & >=0 & <=65535
	} | {
		type:      "FORWARD_DOMAIN"
		ipAddress: string & !=""
	}
}

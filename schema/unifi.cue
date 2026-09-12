package schema

// UniFi Network Integration API config-as-code.
//
// These definitions model the official UniFi Network Integration API
// (https://<console>/proxy/network/integration/v1) as served by UniFi Network
// 10.x. Objects are matched by NAME, never by id: ids are server-assigned and
// must not be committed to a consumer repo. The exceptions are the two object
// kinds whose name is not unique: DNS policies are keyed by type + domain, and
// firewall policies by (sourceZone, destinationZone, name).
//
// Fields mirror the API's own names and enum values so that `unifi export`
// output can be pasted straight into an instance file.

// #Site is the top-level document consumed by `unifi diff` / `unifi sync`.
//
// Every section is optional and carries no default. Omitting a section means
// "not managed by this instance file": `sync --prune` leaves every object of
// that type alone, however many the console holds. Declaring a section, even
// as an empty list, means "managed": `--prune` then deletes every
// USER_DEFINED object of that type the instance file does not list. An
// instance file must set a section to `[]` on purpose to clear it out — see
// docs/unifi.md.
#Site: {
	networks?:         [...#Network]
	firewallZones?:    [...#FirewallZone]
	wifi?:             [...#WiFi]
	firewallPolicies?: [...#FirewallPolicy]
	dnsPolicies?:      [...#DNSPolicy]
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
// **Identity is the triple (sourceZone, destinationZone, name)**, not the name
// alone: the console's own defaults reuse a handful of names across dozens of
// policies ("Allow All Traffic" appears 19 times on a stock 10.6 console). The
// triple was verified unique across all 67 policies of the reference console.
// Two entries sharing it is an error, not a merge.
#FirewallPolicy: {
	name:         string & !=""
	description?: string
	enabled:      bool | *true

	action: "ALLOW" | "BLOCK" | "REJECT"
	// Only meaningful for "ALLOW": permit the reply traffic of a matched flow.
	allowReturnTraffic: bool | *true

	// Names of #FirewallZone entries. Zones the instance file does not declare
	// may still be referenced: they are resolved against the live console.
	sourceZone:      string & !=""
	destinationZone: string & !=""

	// What, within the zone, the policy matches. Omit to match the whole zone.
	source?:      #TrafficFilter
	destination?: #TrafficFilter

	ipVersion: "IPV4" | "IPV6" | "IPV4_AND_IPV6" | *"IPV4_AND_IPV6"

	// IP protocol as the API spells it, upper-case: "TCP", "UDP", "TCP_UDP",
	// "ICMP", "ICMPV6", "GRE", "ESP", … Omit to match every protocol.
	protocol?:             string & !=""
	protocolMatchOpposite: bool | *false

	connectionStates?: [...("NEW" | "INVALID" | "ESTABLISHED" | "RELATED")]
	loggingEnabled: bool | *false

	// When the policy is in force. Omit for "always".
	schedule?: #FirewallSchedule

	// Relative position among the USER_DEFINED policies that share this
	// policy's zone pair — the console orders policies per zone pair, not
	// site-wide. Lower runs first. Omit to leave the policy where it is;
	// SYSTEM_DEFINED policies are never reordered.
	order?: int
}

// #TrafficFilter narrows one end of a policy. `type` names the filter the
// console treats as primary; the other filters may be set alongside it (the
// stock "Allow mDNS" policy matches an IP address set *and* a port).
#TrafficFilter: {
	type: "NETWORK" | "IP_ADDRESS" | "PORT" | "MAC_ADDRESS" | "APPLICATION"

	// Member networks of the zone, by name.
	networkFilter?: {
		networks: [...string]
		// Match everything except the listed networks.
		matchOpposite: bool | *false
	}
	ipAddressFilter?: {
		items: [...#IPAddressMatch]
		matchOpposite: bool | *false
	}
	portFilter?: {
		items: [...#Port]
		matchOpposite: bool | *false
	}
	macAddressFilter?: {
		macAddresses: [...#MACAddress]
	}
	// Numeric application ids as the console's DPI catalogue assigns them.
	// There is no name lookup in the Integration API, so `unifi export` is
	// how you find the id of an application you picked in the UI.
	applicationFilter?: {
		applicationIds: [...int]
	}
}

#IPAddressMatch: {
	type:  "IP_ADDRESS" | "SUBNET"
	value: string & !=""
}

// #MACAddress is colon-separated and lower-case, the form the API returns.
#MACAddress: string & =~"^[0-9a-f]{2}(:[0-9a-f]{2}){5}$"

// #FirewallSchedule limits when a policy is in force. Only "EVERY_DAY" and
// "CUSTOM" were seen on the reference console; the other modes are what the
// console UI offers.
#FirewallSchedule: {
	mode: "EVERY_DAY" | "EVERY_WEEK" | "ONE_TIME_ONLY" | "CUSTOM"

	// 24-hour "HH:MM". A stopTime earlier than startTime wraps past midnight.
	startTime?: string & =~"^([01][0-9]|2[0-3]):[0-5][0-9]$"
	stopTime?:  string & =~"^([01][0-9]|2[0-3]):[0-5][0-9]$"

	repeatOnDays?: [...("MONDAY" | "TUESDAY" | "WEDNESDAY" | "THURSDAY" | "FRIDAY" | "SATURDAY" | "SUNDAY")]

	// "YYYY-MM-DD".
	startDate?: string & =~"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"
	stopDate?:  string & =~"^[0-9]{4}-[0-9]{2}-[0-9]{2}$"
}

// #Port is a single port or an inclusive "start-end" range, written as a
// string. `unifi sync` rejects anything outside 1-65535 rather than sending a
// policy that would match the wrong traffic.
#Port: string & =~"^([1-9][0-9]{0,4})(-[1-9][0-9]{0,4})?$"

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

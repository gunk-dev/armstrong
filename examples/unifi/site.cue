// Example UniFi site instance. Addresses are RFC 5737 documentation ranges.
//
//	cue export ./examples/unifi --out json | unifi diff
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	networks: [
		{
			name:   "Default"
			vlanId: 1
			ipv4: {
				hostIpAddress: "192.0.2.1"
				prefixLength:  24
				dhcp: {
					mode:             "SERVER"
					rangeStart:       "192.0.2.100"
					rangeStop:        "192.0.2.199"
					leaseTimeSeconds: 86400
				}
			}
		},
		{
			name:                  "IoT"
			vlanId:                20
			isolationEnabled:      true
			internetAccessEnabled: true
			ipv4: {
				hostIpAddress: "198.51.100.1"
				prefixLength:  24
				dhcp: {
					mode:             "SERVER"
					rangeStart:       "198.51.100.100"
					rangeStop:        "198.51.100.199"
					leaseTimeSeconds: 3600
					dnsServers: ["203.0.113.53"]
				}
			}
		},
	]

	firewallZones: [
		{name: "internal", networks: ["Default"]},
		{name: "iot", networks: ["IoT"]},
	]

	wifi: [
		{
			name:    "example-main"
			network: "Default"
			security: {
				type:          "WPA2_PERSONAL"
				passphraseEnv: "UNIFI_WIFI_MAIN"
			}
			bands: [2.4, 5]
		},
		{
			name:     "example-iot"
			network:  "IoT"
			hideName: true
			security: {
				type:          "WPA2_PERSONAL"
				passphraseEnv: "UNIFI_WIFI_IOT"
			}
			bands: [2.4]
			clientIsolationEnabled: true
		},
	]

	firewallPolicies: [
		{
			name:            "iot-to-internal-block"
			action:          "BLOCK"
			sourceZone:      "iot"
			destinationZone: "internal"
			order:           10
		},
		// A policy is identified by (sourceZone, destinationZone, name), so
		// this one coexists with the block rule above under a name the
		// console's own defaults also use.
		{
			name:            "internal-to-iot-mgmt"
			action:          "ALLOW"
			sourceZone:      "internal"
			destinationZone: "iot"
			protocol:        "TCP"
			destination: {
				type: "PORT"
				portFilter: items: ["22", "8000-8100"]
			}
			connectionStates: ["NEW", "ESTABLISHED", "RELATED"]
			order: 20
		},
		// Filters, applications and schedules are all first-class: this is
		// "no streaming on the kids' tablets after bedtime".
		{
			name:            "iot-curfew"
			action:          "BLOCK"
			sourceZone:      "iot"
			destinationZone: "internal"
			source: {
				type: "MAC_ADDRESS"
				macAddressFilter: macAddresses: ["02:00:5e:10:00:01", "02:00:5e:10:00:02"]
			}
			schedule: {
				mode:      "CUSTOM"
				startTime: "21:00"
				stopTime:  "07:00"
				repeatOnDays: ["MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "SUNDAY"]
			}
			order: 30
		},
	]

	dnsPolicies: [
		{type: "A_RECORD", domain: "nas.example.internal", ipv4Address: "192.0.2.10"},
		{type: "CNAME_RECORD", domain: "files.example.internal", targetDomain: "nas.example.internal"},
	]
}

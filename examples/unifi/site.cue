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
	]

	dnsPolicies: [
		{type: "A_RECORD", domain: "nas.example.internal", ipv4Address: "192.0.2.10"},
		{type: "CNAME_RECORD", domain: "files.example.internal", targetDomain: "nas.example.internal"},
	]
}

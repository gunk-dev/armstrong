// The #Site the VM test's fake console is seeded to match. Deliberately small
// but structurally identical to examples/unifi/site.cue: a gateway network
// with a DHCP server, two firewall zones, a WPA2 SSID whose passphrase comes
// from the environment, no firewall policies, and one DNS record.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	networks: [
		{
			name:                  "Default"
			management:            "GATEWAY"
			enabled:               true
			vlanId:                1
			isolationEnabled:      false
			internetAccessEnabled: true
			cellularBackupEnabled: false
			mdnsForwardingEnabled: true
			ipv4: {
				hostIpAddress:    "10.0.0.1"
				prefixLength:     24
				autoScaleEnabled: false
				dhcp: {
					mode:                         "SERVER"
					rangeStart:                   "10.0.0.10"
					rangeStop:                    "10.0.0.200"
					leaseTimeSeconds:             86400
					domainName:                   "test.invalid"
					pingConflictDetectionEnabled: true
				}
			}
		},
	]

	firewallZones: [
		{name: "Internal", networks: ["Default"]},
		{name: "Gateway", networks: []},
	]

	wifi: [
		{
			name:    "test-ssid"
			enabled: true
			network: "NATIVE"
			security: {
				type:          "WPA2_PERSONAL"
				passphraseEnv: "UNIFI_PSK_TEST"
			}
			bands: [2.4, 5]
			clientIsolationEnabled:              false
			hideName:                            false
			multicastToUnicastConversionEnabled: true
			uapsdEnabled:                        false
		},
	]

	// Left empty so the test covers that path: an empty list must never prune.
	firewallPolicies: []

	dnsPolicies: [
		{
			type:        "A_RECORD"
			enabled:     true
			domain:      "host.test.invalid"
			ipv4Address: "10.0.0.5"
			ttlSeconds:  0
		},
	]
}

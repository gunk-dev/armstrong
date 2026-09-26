// An IPv6 address in an IPV4 policy: the console refuses it. TestLint requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	firewallPolicies: [{
		name:            "dns"
		action:          "ALLOW"
		sourceZone:      "iot"
		destinationZone: "internal"
		ipVersion:       "IPV4"
		destination: {
			type: "IP_ADDRESS"
			ipAddressFilter: items: [{type: "IP_ADDRESS", value: "fd00::53"}]
		}
	}]
}

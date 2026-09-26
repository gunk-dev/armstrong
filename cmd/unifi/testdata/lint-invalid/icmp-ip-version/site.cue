// ICMP declared for both IP versions: the console takes ICMP only with IPV4. TestLint requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	firewallPolicies: [{
		name:            "icmp block"
		action:          "BLOCK"
		sourceZone:      "iot"
		destinationZone: "internal"
		protocol:        "ICMP"
	}]
}

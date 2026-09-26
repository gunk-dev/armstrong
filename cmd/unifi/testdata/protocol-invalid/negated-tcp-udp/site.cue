// A negated TCP_UDP: the console refuses matchOpposite on the PRESET filter TCP_UDP is sent as. TestCueVetRejectsNegatedPreset requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	firewallPolicies: [{
		name:                  "other protocol block"
		action:                "BLOCK"
		sourceZone:            "IoT"
		destinationZone:       "Servers"
		protocol:              "TCP_UDP"
		protocolMatchOpposite: true
	}]
}

// Return traffic allowed on a policy to the built-in Gateway zone: the console refuses it. TestLint requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	firewallPolicies: [{
		name:               "guest dns"
		action:             "ALLOW"
		allowReturnTraffic: true
		sourceZone:         "guest"
		destinationZone:    "Gateway"
		protocol:           "UDP"
	}]
}

// An ALLOW of return traffic that also allows return traffic: the console refuses it. TestLint requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	firewallPolicies: [{
		name:               "replies"
		action:             "ALLOW"
		allowReturnTraffic: true
		sourceZone:         "media"
		destinationZone:    "guest"
		connectionStates: ["ESTABLISHED", "RELATED"]
	}]
}

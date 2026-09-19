// Participation is each network's mdnsForwardingEnabled, not a list on #MDNS. TestCueVetMDNS requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	mdns: {mode: "all", networks: ["IoT"]}
}

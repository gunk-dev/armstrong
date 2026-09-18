// An empty networks list is invalid; "off" is how to say no networks. TestCueVetMDNS requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	mdns: {mode: "auto", networks: []}
}

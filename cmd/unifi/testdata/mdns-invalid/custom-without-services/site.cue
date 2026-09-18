// custom with nothing on its allow-list is invalid. TestCueVetMDNS requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	mdns: {mode: "custom"}
}

// auto reflects the whole catalogue, so a service list is invalid. TestCueVetMDNS requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	mdns: {mode: "auto", services: ["printers"]}
}

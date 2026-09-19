// There is no mode off: the proxy is off when no network has mdnsForwardingEnabled. TestCueVetMDNS requires `cue vet -c` to reject it.
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	mdns: {mode: "off"}
}

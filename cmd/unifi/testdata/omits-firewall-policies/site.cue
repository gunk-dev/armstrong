// A minimal #Site instance that deliberately never mentions firewallPolicies,
// for TestCueExportOmitsUndeclaredSections: cue export of this file must not
// emit a "firewallPolicies" key at all, proving an omitted section is absent
// from the JSON rather than defaulting to "[]" the way it did before #Site's
// sections became optional (see schema/unifi.cue and docs/unifi.md).
package unifi

import "gunk.dev/armstrong/schema"

site: schema.#Site & {
	networks: [
		{name: "Default", vlanId: 1},
	]
	firewallZones: [
		{name: "internal", networks: ["Default"]},
	]
	wifi: []
	dnsPolicies: []
}

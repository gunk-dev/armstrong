package main

import (
	"cmp"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// The console stores a policy's list-valued fields as sets: it reads back
// port items, IP address items, connection states and schedule days in an
// order of its own (["53", "853"] comes back as [853, 53]), and a PUT in any
// other order does not stick. canonicalPolicy puts every such list into one
// fixed order so that comparison is order-insensitive and `export` is
// deterministic. Network ids, MAC addresses and application ids are sets by
// meaning and get the same treatment.
//
// Duplicates carry no meaning in a set, so they are dropped: a list declaring
// the same port twice matches a console that holds it once, and vice versa.
//
// The result shares no slices with the input.
func canonicalPolicy(p firewallPolicy) firewallPolicy {
	p.ConnectionStates = canonicalSet(p.ConnectionStates, strings.Compare)
	p.Source = canonicalFilter(p.Source)
	p.Destination = canonicalFilter(p.Destination)
	if s := p.Schedule; s != nil {
		c := *s
		c.RepeatOnDays = canonicalSet(s.RepeatOnDays, compareDays)
		p.Schedule = &c
	}
	return p
}

func canonicalFilter(f *trafficFilter) *trafficFilter {
	if f == nil {
		return nil
	}
	out := *f
	if n := f.NetworkFilter; n != nil {
		out.NetworkFilter = &networkFilter{
			Networks:      canonicalSet(n.Networks, strings.Compare),
			MatchOpposite: n.MatchOpposite,
		}
	}
	if a := f.IPAddressFilter; a != nil {
		out.IPAddressFilter = &ipAddressFilter{
			Items:         canonicalSet(a.Items, compareIPItems),
			MatchOpposite: a.MatchOpposite,
		}
	}
	if pf := f.PortFilter; pf != nil {
		out.PortFilter = &portFilter{
			Items:         canonicalSet(pf.Items, comparePorts),
			MatchOpposite: pf.MatchOpposite,
		}
	}
	if m := f.MACAddressFilter; m != nil {
		out.MACAddressFilter = &macAddressFilter{MACAddresses: canonicalSet(m.MACAddresses, strings.Compare)}
	}
	if ap := f.ApplicationFilter; ap != nil {
		out.ApplicationFilter = &applicationFilter{ApplicationIDs: canonicalSet(ap.ApplicationIDs, cmp.Compare[int])}
	}
	return &out
}

// canonicalSet returns a sorted, de-duplicated copy of s. An empty list stays
// as it was (nil or empty), since the JSON shape of an empty list is not this
// function's concern.
func canonicalSet[T any](s []T, compare func(a, b T) int) []T {
	if len(s) == 0 {
		return s
	}
	out := slices.Clone(s)
	slices.SortFunc(out, compare)
	return slices.CompactFunc(out, func(a, b T) bool { return compare(a, b) == 0 })
}

// comparePorts orders #Port strings numerically by start then end, so "53"
// sorts before "853" and "1000". A string that does not parse sorts last.
func comparePorts(a, b string) int {
	as, ae, aok := portBounds(a)
	bs, be, bok := portBounds(b)
	switch {
	case aok && bok:
		return cmp.Or(cmp.Compare(as, bs), cmp.Compare(ae, be), strings.Compare(a, b))
	case aok:
		return -1
	case bok:
		return 1
	}
	return strings.Compare(a, b)
}

func portBounds(s string) (start, end int, ok bool) {
	if start, end, ok := splitPortRange(s); ok {
		return start, end, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, n, err == nil
}

// compareIPItems orders by item type, then by address: 9.0.0.0/8 sorts before
// 10.0.0.0/8, and IPv4 before IPv6. A value that does not parse as an address
// or prefix falls back to plain string order after the ones that do.
func compareIPItems(a, b ipAddressMatch) int {
	if c := strings.Compare(a.Type, b.Type); c != 0 {
		return c
	}
	ap, aok := parseIPValue(a.Value)
	bp, bok := parseIPValue(b.Value)
	switch {
	case aok && bok:
		return cmp.Or(ap.Addr().Compare(bp.Addr()), cmp.Compare(ap.Bits(), bp.Bits()), strings.Compare(a.Value, b.Value))
	case aok:
		return -1
	case bok:
		return 1
	}
	return strings.Compare(a.Value, b.Value)
}

func parseIPValue(s string) (netip.Prefix, bool) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

var weekdays = map[string]int{
	"MONDAY": 1, "TUESDAY": 2, "WEDNESDAY": 3, "THURSDAY": 4, "FRIDAY": 5, "SATURDAY": 6, "SUNDAY": 7,
}

// compareDays orders schedule days Monday first; an unknown name sorts last.
func compareDays(a, b string) int {
	da, db := weekdays[a], weekdays[b]
	if da == 0 {
		da = 8
	}
	if db == 0 {
		db = 8
	}
	return cmp.Or(cmp.Compare(da, db), strings.Compare(a, b))
}

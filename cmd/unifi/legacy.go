package main

// DHCP reservations are the only object type cmd/unifi manages through the
// legacy controller API (/proxy/network/api/s/{site}/…) rather than the
// Integration API. The Integration API has no reservation surface: its
// /clients routes are GET-only, list only connected clients and carry no
// fixed-IP field (see docs/unifi-api-notes.md). The legacy API accepts the
// same X-API-KEY, lists every known client including offline ones, and
// accepts writes. It is undocumented and can change without notice, so
// everything that speaks it lives in this file and nothing else does.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strings"
)

const legacyPrefix = "/proxy/network/api/s/"

// legacyEnvelope is how every legacy response is wrapped. Application errors
// come back as {"meta":{"rc":"error","msg":"api.err.…"},"data":[]}.
type legacyEnvelope struct {
	Meta struct {
		RC  string `json:"rc"`
		Msg string `json:"msg"`
	} `json:"meta"`
	Data []json.RawMessage `json:"data"`
}

// legacyDo issues a request against the legacy API of one site and returns
// the envelope's data. site is the legacy site name — the Integration API
// site's internalReference, e.g. "default".
func (c *client) legacyDo(method, site, path string, body any) ([]json.RawMessage, error) {
	data, err := c.send(method, legacyPrefix+site, path, body)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			var env legacyEnvelope
			if json.Unmarshal([]byte(apiErr.Body), &env) == nil {
				apiErr.Code = env.Meta.Msg
			}
		}
		return nil, err
	}
	var env legacyEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("%s %s: parse response: %w", method, path, err)
	}
	if env.Meta.RC != "ok" {
		return nil, fmt.Errorf("%s %s: console answered rc=%q: %s", method, path, env.Meta.RC, redact(env.Meta.Msg))
	}
	return env.Data, nil
}

// legacyUser is a `rest/user` entry: one client the console knows, connected
// or not. Unset fields are absent from the entry rather than null.
type legacyUser struct {
	ID       string `json:"_id"`
	MAC      string `json:"mac"`
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// UseFixedIP is the switch that decides whether a reservation exists. A
	// FixedIP left behind while it is false is not a reservation.
	UseFixedIP bool   `json:"use_fixedip"`
	FixedIP    string `json:"fixed_ip,omitempty"`
	// NetworkID is only stored when someone set it; when absent the console
	// applies the reservation on the client's current network.
	NetworkID               string `json:"network_id,omitempty"`
	LastConnectionNetworkID string `json:"last_connection_network_id,omitempty"`
}

// legacyState is what reconciling reservations needs from the legacy API.
type legacyState struct {
	users []legacyUser
	// networkNames maps a legacy network `_id` to its name. Legacy ids are
	// Mongo ObjectIds and do not match the Integration API's UUIDs, so name
	// is the only join between the two.
	networkNames nameLookup
	// liveNetwork maps a connected client's MAC to the network `stat/sta`
	// reports it on.
	liveNetwork map[string]string
}

func (c *client) legacyState(site string) (*legacyState, error) {
	raws, err := c.legacyDo(http.MethodGet, site, "/rest/user", nil)
	if err != nil {
		return nil, fmt.Errorf("list clients (legacy rest/user): %w", err)
	}
	st := &legacyState{networkNames: nameLookup{}, liveNetwork: map[string]string{}}
	for _, raw := range raws {
		var u legacyUser
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, fmt.Errorf("parse client: %w", err)
		}
		u.MAC = strings.ToLower(u.MAC)
		st.users = append(st.users, u)
	}

	raws, err = c.legacyDo(http.MethodGet, site, "/rest/networkconf", nil)
	if err != nil {
		return nil, fmt.Errorf("list networks (legacy rest/networkconf): %w", err)
	}
	for _, raw := range raws {
		var n struct {
			ID   string `json:"_id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("parse network: %w", err)
		}
		st.networkNames[n.ID] = n.Name
	}

	raws, err = c.legacyDo(http.MethodGet, site, "/stat/sta", nil)
	if err != nil {
		return nil, fmt.Errorf("list connected clients (legacy stat/sta): %w", err)
	}
	for _, raw := range raws {
		var s struct {
			MAC       string `json:"mac"`
			NetworkID string `json:"network_id"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("parse connected client: %w", err)
		}
		st.liveNetwork[strings.ToLower(s.MAC)] = s.NetworkID
	}
	return st, nil
}

// networkName is the name of the network a reservation applies on: the
// stored network_id when there is one, otherwise the network the client is
// on now, otherwise the one it was last seen on. "" means it cannot be told.
func (st *legacyState) networkName(u legacyUser) string {
	id := u.NetworkID
	if id == "" {
		id = st.liveNetwork[u.MAC]
	}
	if id == "" {
		id = u.LastConnectionNetworkID
	}
	return st.networkNames.name(id)
}

// reservations lists the entries holding an active reservation, by MAC.
func (st *legacyState) reservations() []legacyUser {
	var out []legacyUser
	for _, u := range st.users {
		if u.UseFixedIP {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MAC < out[j].MAC })
	return out
}

// exportReservations renders the live reservations. One whose network
// cannot be named is left out and reported, so that export stays a no-op
// when fed back into diff.
func exportReservations(st *legacyState, warn io.Writer) []reservation {
	out := []reservation{}
	for _, u := range st.reservations() {
		network := st.networkName(u)
		if network == "" {
			fmt.Fprintf(warn, "skipping reservation %s: it stores no network and the client has never "+
				"been seen on one, so there is no network to name.\n", u.MAC)
			continue
		}
		out = append(out, reservation{MAC: u.MAC, Name: u.Name, FixedIP: u.FixedIP, Network: network})
	}
	return out
}

// label is how a reservation is named in the plan: "<name> <mac>", or just
// the MAC when there is no name.
func (res reservation) label() string {
	if res.Name == "" {
		return res.key()
	}
	return res.Name + " " + res.key()
}

// mutateLegacy is mutate for the legacy API: a no-op on a dry run, which
// includes diff and the planning pass of sync.
func (r *reconciler) mutateLegacy(method, path string, body any) error {
	if r.dryRun {
		return nil
	}
	_, err := r.client.legacyDo(method, r.legacySite, path, body)
	return err
}

// syncReservations reconciles DHCP reservations. An absent section is not
// managed and the legacy API is not even read. Pruning clears use_fixedip and
// never forgets the client record: forget-sta would also drop the client's
// name, history and every other setting. A clear is a deletion to the guards:
// it needs "reservation <mac>" in `deletions` and counts towards
// --max-changes, as an update does; a create does not count. All writes go
// through mutateLegacy, so the planning pass of a sync makes none of them.
func (r *reconciler) syncReservations() error {
	if r.want.Reservations == nil {
		return nil
	}
	if err := checkReservations(r.want.Reservations); err != nil {
		return err
	}
	st, err := r.client.legacyState(r.legacySite)
	if err != nil {
		return err
	}
	byMAC := map[string]legacyUser{}
	for _, u := range st.users {
		byMAC[u.MAC] = u
	}
	netIDs := map[string]string{}
	for id, name := range st.networkNames {
		netIDs[name] = id
	}
	declaredNetworks := map[string]bool{}
	for _, n := range r.want.Networks {
		declaredNetworks[n.Name] = true
	}

	seen := map[string]bool{}
	for _, want := range r.want.Reservations {
		want.MAC = want.key()
		seen[want.MAC] = true

		networkID, ok := netIDs[want.Network]
		if !ok {
			// A network created earlier in this run does not exist under
			// --dry-run, so its id cannot be known yet.
			if !r.dryRun || !declaredNetworks[want.Network] {
				return fmt.Errorf("reservation %s references unknown network %q", want.MAC, want.Network)
			}
			networkID = pendingID
		}
		body := map[string]any{"use_fixedip": true, "fixed_ip": want.FixedIP, "network_id": networkID}
		if want.Name != "" {
			body["name"] = want.Name
		}

		got, known := byMAC[want.MAC]
		target := fmt.Sprintf("%s -> %s", want.label(), want.FixedIP)
		switch {
		case !known:
			body["mac"] = want.MAC
			r.logf("CREATE", "reservation", target, "%s", want.Network)
			if err := r.mutateLegacy(http.MethodPost, "/rest/user", body); err != nil {
				return fmt.Errorf("create reservation %s: %w", want.MAC, err)
			}
		case !got.UseFixedIP:
			// The console already knows the client; the reservation goes on
			// its existing record.
			r.logf("CREATE", "reservation", target, "%s", want.Network)
			if err := r.mutateLegacy(http.MethodPut, "/rest/user/"+got.ID, body); err != nil {
				return fmt.Errorf("create reservation %s: %w", want.MAC, err)
			}
		default:
			changes := reservationChanges(st, got, want)
			if len(changes) == 0 {
				r.logf("OK", "reservation", target, "%s", want.Network)
				continue
			}
			r.logf("UPDATE", "reservation", target, "%s; %s", want.Network, strings.Join(changes, ", "))
			if err := r.mutateLegacy(http.MethodPut, "/rest/user/"+got.ID, body); err != nil {
				return fmt.Errorf("update reservation %s: %w", want.MAC, err)
			}
		}
	}

	if !r.prune {
		return nil
	}
	for _, got := range st.reservations() {
		if seen[got.MAC] {
			continue
		}
		// The plan line leads with the bare MAC, since that is the identity
		// `deletions` lists: "reservation <mac>".
		network := st.networkName(got)
		if network == "" {
			network = "unknown network"
		}
		detail := got.FixedIP + " on " + network
		if got.Name != "" {
			detail = got.Name + " -> " + detail
		}
		ok, err := r.approveDeletion("reservation", got.MAC, detail)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := r.mutateLegacy(http.MethodPut, "/rest/user/"+got.ID, map[string]any{"use_fixedip": false}); err != nil {
			return fmt.Errorf("clear reservation %s: %w", got.MAC, err)
		}
	}
	return nil
}

// reservationChanges describes how a live reservation differs from the
// desired one. A stored network_id that is absent counts as matching only
// when the network the client is actually on agrees; otherwise the update
// sets it explicitly. A name the instance file leaves out is not compared.
func reservationChanges(st *legacyState, got legacyUser, want reservation) []string {
	var changes []string
	if got.FixedIP != want.FixedIP {
		changes = append(changes, fmt.Sprintf("fixedIp %s -> %s", got.FixedIP, want.FixedIP))
	}
	if have := st.networkName(got); have != want.Network {
		if have == "" {
			have = "unknown"
		}
		changes = append(changes, fmt.Sprintf("network %s -> %s", have, want.Network))
	}
	if want.Name != "" && got.Name != want.Name {
		changes = append(changes, "name")
	}
	return changes
}

// checkReservations rejects an instance file that declares one MAC or one
// address twice, or an address that is not IPv4.
func checkReservations(res []reservation) error {
	macs, ips := map[string]bool{}, map[string]bool{}
	for _, r := range res {
		if macs[r.key()] {
			return fmt.Errorf("two reservations share the MAC address %s", r.key())
		}
		macs[r.key()] = true
		if addr, err := netip.ParseAddr(r.FixedIP); err != nil || !addr.Is4() {
			return fmt.Errorf("reservation %s: fixedIp %q is not an IPv4 address", r.key(), r.FixedIP)
		}
		if ips[r.FixedIP] {
			return fmt.Errorf("two reservations share the address %s", r.FixedIP)
		}
		ips[r.FixedIP] = true
	}
	return nil
}

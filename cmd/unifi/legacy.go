package main

// DHCP reservations and the mDNS proxy setting are the only things cmd/unifi
// manages through the legacy controller API (/proxy/network/api/s/{site}/…)
// rather than the Integration API, which has no surface for either: its
// /clients routes are GET-only, list only connected clients and carry no
// fixed-IP field, and it exposes only the per-network mDNS participation flag
// (see docs/unifi-api-notes.md). The legacy API accepts the same X-API-KEY,
// lists every known client including offline ones, and accepts writes. It is
// undocumented and can change without notice, so everything that speaks it
// lives in this file and nothing else does.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
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
	// networkNames maps a legacy network `_id` to its name; see
	// legacyNetworks.
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
	st := &legacyState{liveNetwork: map[string]string{}}
	for _, raw := range raws {
		var u legacyUser
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, fmt.Errorf("parse client: %w", err)
		}
		u.MAC = strings.ToLower(u.MAC)
		st.users = append(st.users, u)
	}

	if st.networkNames, err = c.legacyNetworks(site); err != nil {
		return nil, err
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

// legacyNetworks maps each legacy network `_id` to its name. Legacy ids are
// Mongo ObjectIds and do not match the Integration API's UUIDs, so name is the
// only join between the two.
func (c *client) legacyNetworks(site string) (nameLookup, error) {
	raws, err := c.legacyDo(http.MethodGet, site, "/rest/networkconf", nil)
	if err != nil {
		return nil, fmt.Errorf("list networks (legacy rest/networkconf): %w", err)
	}
	names := nameLookup{}
	for _, raw := range raws {
		var n struct {
			ID   string `json:"_id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("parse network: %w", err)
		}
		names[n.ID] = n.Name
	}
	return names, nil
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

// ------------------------------------------------------------ mDNS proxy

// legacyMDNS is the `rest/setting` entry keyed "mdns": the gateway's one
// mDNS proxy. raw is the entry as read, which a write echoes so that fields
// this tool does not model survive it.
type legacyMDNS struct {
	ID                   string   `json:"_id"`
	Mode                 string   `json:"mode"`
	EnabledFor           string   `json:"enabled_for"`
	EnabledForNetworkIDs []string `json:"enabled_for_network_ids"`
	// The allow-list in custom mode; the whole catalogue, informational only,
	// in auto.
	PredefinedServices []struct {
		Code string `json:"code"`
	} `json:"predefined_services"`
	CustomServices []json.RawMessage `json:"custom_services"`

	raw map[string]any
}

// legacyMDNSEnabledForCustom is the enabled_for value that makes
// enabled_for_network_ids the participating set. UNCONFIRMED: only "all" has
// been seen on a console.
const legacyMDNSEnabledForCustom = "custom"

// customServiceName reads one custom_services element. UNCONFIRMED: assumed
// to be {"name":"_hap._tcp"}; only an empty list has been seen. Anything else,
// extra keys included, is refused rather than rewritten without them.
func customServiceName(raw json.RawMessage) (string, error) {
	var e struct {
		Name string `json:"name"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil || e.Name == "" {
		return "", fmt.Errorf(`mdns proxy custom service %s is not the {"name": "_service._proto"} shape this tool assumes`, raw)
	}
	return e.Name, nil
}

// customServiceEntry is the inverse of customServiceName, and as unconfirmed.
func customServiceEntry(name string) map[string]any { return map[string]any{"name": name} }

// legacyMDNS reads the proxy setting from the settings list; the per-key
// GET rest/setting/mdns has never been observed, so it is not relied on.
func (c *client) legacyMDNS(site string) (*legacyMDNS, error) {
	raws, err := c.legacyDo(http.MethodGet, site, "/rest/setting", nil)
	if err != nil {
		return nil, fmt.Errorf("list settings (legacy rest/setting): %w", err)
	}
	var found []*legacyMDNS
	for _, raw := range raws {
		var k struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(raw, &k); err != nil {
			return nil, fmt.Errorf("parse setting: %w", err)
		}
		if k.Key != "mdns" {
			continue
		}
		m := &legacyMDNS{}
		if err := json.Unmarshal(raw, m); err != nil {
			return nil, fmt.Errorf("parse mdns setting: %w", err)
		}
		// UseNumber: an echoed number must not come back as a rounded float.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&m.raw); err != nil {
			return nil, fmt.Errorf("parse mdns setting: %w", err)
		}
		found = append(found, m)
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("legacy rest/setting holds %d mdns entries; expected exactly one", len(found))
	}
	return found[0], nil
}

// project reads the live setting as #MDNS. Anything it cannot read with
// certainty is an error, never a guess: a guess would plan a PUT over it.
func (m *legacyMDNS) project(names nameLookup) (mdns, error) {
	out := mdns{Mode: m.Mode}
	switch m.Mode {
	case "auto", "off":
	case "custom":
		for _, s := range m.PredefinedServices {
			out.Services = append(out.Services, s.Code)
		}
		for _, raw := range m.CustomServices {
			name, err := customServiceName(raw)
			if err != nil {
				return mdns{}, err
			}
			out.CustomServices = append(out.CustomServices, name)
		}
		sort.Strings(out.Services)
		sort.Strings(out.CustomServices)
	default:
		return mdns{}, fmt.Errorf("mdns proxy mode %q is not one #MDNS models", m.Mode)
	}
	switch m.EnabledFor {
	case "all":
	case legacyMDNSEnabledForCustom:
		if len(m.EnabledForNetworkIDs) == 0 {
			return mdns{}, errors.New("mdns proxy is restricted to a list of networks, but the list is empty")
		}
		for _, id := range m.EnabledForNetworkIDs {
			name := names.name(id)
			if name == "" {
				return mdns{}, fmt.Errorf("mdns proxy network %q is not in legacy rest/networkconf", id)
			}
			out.Networks = append(out.Networks, name)
		}
		sort.Strings(out.Networks)
	default:
		return mdns{}, fmt.Errorf("mdns proxy enabled_for %q is not one #MDNS models", m.EnabledFor)
	}
	if err := checkMDNS(&out); err != nil {
		return mdns{}, fmt.Errorf("live %w", err)
	}
	return out, nil
}

var customServiceRE = regexp.MustCompile(`^_[a-z0-9-]+\._(tcp|udp)$`)

// checkMDNS enforces #MDNS on input that skipped `cue vet`, and on the live
// projection, so that export never prints what vet would reject.
func checkMDNS(m *mdns) error {
	switch m.Mode {
	case "custom":
		if len(m.Services)+len(m.CustomServices) == 0 {
			return errors.New("mdns proxy: mode custom needs services or customServices")
		}
	case "auto", "off":
		if m.Services != nil || m.CustomServices != nil {
			return fmt.Errorf("mdns proxy: mode %s takes no services or customServices", m.Mode)
		}
	default:
		return fmt.Errorf("mdns proxy: mode %q is not auto, custom or off", m.Mode)
	}
	if m.Networks != nil && len(m.Networks) == 0 {
		return errors.New(`mdns proxy: networks is empty; use mode "off" for no networks`)
	}
	for _, list := range []struct {
		field  string
		values []string
	}{{"services", m.Services}, {"customServices", m.CustomServices}, {"networks", m.Networks}} {
		seen := map[string]bool{}
		for _, v := range list.values {
			switch {
			case v == "":
				return fmt.Errorf("mdns proxy: empty entry in %s", list.field)
			case list.field == "customServices" && !customServiceRE.MatchString(v):
				return fmt.Errorf("mdns proxy: custom service %q is not _service._tcp or _service._udp", v)
			case seen[v]:
				return fmt.Errorf("mdns proxy: %s lists %q twice", list.field, v)
			}
			seen[v] = true
		}
	}
	return nil
}

// syncMDNS reconciles the site-wide mDNS proxy. An absent mdns is not managed
// and the legacy API is not read. It is a singleton, only ever updated, so
// --prune and `deletions` do not apply; an update counts towards
// --max-changes. It runs before syncNetworks (see run), so every network it
// names must already exist on the console.
func (r *reconciler) syncMDNS() error {
	want := r.want.MDNS
	if want == nil {
		return nil
	}
	if err := checkMDNS(want); err != nil {
		return err
	}
	names, err := r.client.legacyNetworks(r.legacySite)
	if err != nil {
		return err
	}
	got, err := r.client.legacyMDNS(r.legacySite)
	if err != nil {
		return err
	}
	have, err := got.project(names)
	if err != nil {
		return fmt.Errorf("%w; refusing to manage an mdns proxy setting this tool cannot read faithfully", err)
	}

	netIDs := map[string]string{}
	for id, name := range names {
		netIDs[name] = id
	}
	declared := map[string]bool{}
	for _, n := range r.want.Networks {
		declared[n.Name] = true
	}
	var ids []string
	for _, n := range want.Networks {
		id, ok := netIDs[n]
		if !ok {
			return fmt.Errorf("mdns proxy references unknown network %q; create it in a sync of its own first", n)
		}
		// A declared networks section is every network the file keeps.
		if r.want.Networks != nil && !declared[n] {
			return fmt.Errorf("mdns proxy references network %q, which the networks section does not declare", n)
		}
		ids = append(ids, id)
	}

	changes := mdnsChanges(have, *want)
	if len(changes) == 0 {
		r.logf("OK", "mdns proxy", want.Mode, "%s", want.summary())
		return nil
	}
	if got.ID == "" {
		return errors.New("the console's mdns setting has no _id to update")
	}
	r.logf("UPDATE", "mdns proxy", want.Mode, "%s", strings.Join(changes, "; "))
	// UNCONFIRMED path; see mdnsWriteBody.
	if err := r.mutateLegacy(http.MethodPut, "/rest/setting/mdns/"+got.ID, mdnsWriteBody(got, *want, ids)); err != nil {
		return fmt.Errorf("update mdns proxy: %w", err)
	}
	return nil
}

// mdnsWriteBody is the PUT to rest/setting/mdns/{_id} that makes the setting
// want: the entry as read, minus `_id`, with the modelled fields replaced.
// UNCONFIRMED: path and full-object body are the classic controller
// convention for settings, not yet seen on a console; echoing every field
// read is what keeps a full-object PUT from dropping any.
func mdnsWriteBody(got *legacyMDNS, want mdns, networkIDs []string) map[string]any {
	body := map[string]any{}
	for k, v := range got.raw {
		if k != "_id" {
			body[k] = v
		}
	}
	body["mode"] = want.Mode
	// Outside custom the lists go back as read: what the console keeps there
	// after leaving custom is unknown.
	if want.Mode == "custom" {
		predefined := []map[string]any{}
		for _, code := range sortedCopy(want.Services) {
			predefined = append(predefined, map[string]any{"code": code})
		}
		custom := []map[string]any{}
		for _, name := range sortedCopy(want.CustomServices) {
			custom = append(custom, customServiceEntry(name))
		}
		body["predefined_services"], body["custom_services"] = predefined, custom
	}
	if want.Networks == nil {
		body["enabled_for"], body["enabled_for_network_ids"] = "all", []string{}
	} else {
		body["enabled_for"], body["enabled_for_network_ids"] = legacyMDNSEnabledForCustom, networkIDs
	}
	return body
}

// mdnsChanges describes how have differs from want, for the plan line. The
// service lists are compared only when want is custom: in auto they are the
// console's catalogue, and off ignores them.
func mdnsChanges(have, want mdns) []string {
	var out []string
	if have.Mode != want.Mode {
		out = append(out, fmt.Sprintf("mode %s -> %s", have.Mode, want.Mode))
	}
	if want.Mode == "custom" {
		if d := setChanges(have.Services, want.Services); d != "" {
			out = append(out, "services "+d)
		}
		if d := setChanges(have.CustomServices, want.CustomServices); d != "" {
			out = append(out, "customServices "+d)
		}
	}
	if (have.Networks == nil) != (want.Networks == nil) || setChanges(have.Networks, want.Networks) != "" {
		out = append(out, fmt.Sprintf("networks %s -> %s", mdnsNetworks(have.Networks), mdnsNetworks(want.Networks)))
	}
	return out
}

// setChanges is "+added -removed", each sorted, or "" for equal sets.
func setChanges(have, want []string) string {
	var out []string
	for _, v := range sortedCopy(want) {
		if !slices.Contains(have, v) {
			out = append(out, "+"+v)
		}
	}
	for _, v := range sortedCopy(have) {
		if !slices.Contains(want, v) {
			out = append(out, "-"+v)
		}
	}
	return strings.Join(out, " ")
}

func mdnsNetworks(names []string) string {
	if names == nil {
		return "all"
	}
	return strings.Join(names, ", ")
}

// summary is the OK line's detail: the lists, as the instance file orders them.
func (m mdns) summary() string {
	var out []string
	if len(m.Services) > 0 {
		out = append(out, "services "+strings.Join(m.Services, ", "))
	}
	if len(m.CustomServices) > 0 {
		out = append(out, "customServices "+strings.Join(m.CustomServices, ", "))
	}
	if m.Networks != nil {
		out = append(out, "networks "+strings.Join(m.Networks, ", "))
	}
	return strings.Join(out, "; ")
}

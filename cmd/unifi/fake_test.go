package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeConsole is an in-memory stand-in for the UniFi Network Integration API.
// It reproduces the behaviours cmd/unifi actually depends on, as recorded in
// docs/unifi-api-notes.md: the API key header, offset/limit paging capped at
// 200, list responses that are overviews rather than full objects,
// server-assigned ids and metadata, and the 400 the zone-based firewall
// endpoints return on a console still running the legacy firewall.
type fakeConsole struct {
	*httptest.Server

	apiKey string
	siteID string

	// zbfConfigured mirrors a console with the zone-based firewall enabled.
	// When false, both firewall endpoints fail the way a live 10.6 console does.
	zbfConfigured bool
	// firewallFault, when set, makes the firewall endpoints fail with some
	// other error — used to check that only the specific "not configured"
	// code is treated as "feature unavailable".
	firewallFault *fault
	// wifiPutFault makes a wifi update fail with a 400 that echoes the request
	// body back, the way a validating API does — so the response contains the
	// passphrase that was just sent.
	wifiPutFault bool
	// omitUserPolicyIDs reproduces a console holding policies that predate the
	// zone-based firewall migration: those come back without an `id`, leaving
	// nothing to PUT or DELETE against. Policies created through the API do
	// carry ids, so the fake does not do this by default — the rest of the
	// policy write path would be untestable otherwise — and the tests that
	// care switch it on explicitly.
	omitUserPolicyIDs bool
	// onMutation, when set, runs as each non-GET request arrives and before
	// it is applied — so a test can inspect the world as of the first write.
	onMutation func(mutation)
	// onLegacyRequest, when set, runs as each legacy request arrives, reads
	// included, before it is served.
	onLegacyRequest func(method, rest string)
	// mdnsWriteRefused makes POST set/setting/mdns answer
	// api.err.InvalidValue whatever the body.
	mdnsWriteRefused bool

	mu   sync.Mutex
	coll map[string]*collection
	// The legacy controller API, which cmd/unifi uses for DHCP reservations
	// and the mDNS proxy. users is `rest/user` by `_id`, in insertion order;
	// stations is `stat/sta` as MAC -> legacy network id; settings is
	// `rest/setting`. `rest/networkconf` is derived from the networks
	// collection, with ids deliberately unlike the Integration API's, as on a
	// real console.
	users     map[string]map[string]any
	userOrder []string
	stations  map[string]string
	settings  []map[string]any
	// legacyRequests records every legacy request, reads included, so a test
	// can assert that an absent section never reached the legacy API.
	legacyRequests []string
	// mutations records every non-GET request, so a test can assert that a
	// dry run touched nothing.
	mutations []mutation
	nextID    int
}

// fault is a canned error response.
type fault struct {
	status  int
	code    string
	message string
}

type mutation struct {
	Method string
	Path   string
	Body   map[string]any
}

// collection is one resource type: objects keyed by id, plus the insertion
// order the list endpoint reports (which for firewall policies is also the
// evaluation order).
type collection struct {
	order []string
	byID  map[string]map[string]any
}

func (c *collection) list() []map[string]any {
	out := make([]map[string]any, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.byID[id])
	}
	return out
}

const (
	collNetworks = "networks"
	collWiFi     = "wifi/broadcasts"
	collDNS      = "dns/policies"
	collZones    = "firewall/zones"
	collPolicies = "firewall/policies"
)

func newFakeConsole(t *testing.T) *fakeConsole {
	t.Helper()
	f := &fakeConsole{
		apiKey:        "test-api-key",
		siteID:        "site-0001",
		zbfConfigured: true,
		coll:          map[string]*collection{},
		users:         map[string]map[string]any{},
		stations:      map[string]string{},
		settings: []map[string]any{
			{"key": "mgmt", "_id": "setting-mgmt", "site_id": fakeLegacySiteID, "led_enabled": true},
			defaultMDNSSetting(),
		},
	}
	for _, name := range []string{collNetworks, collWiFi, collDNS, collZones, collPolicies} {
		f.coll[name] = &collection{byID: map[string]map[string]any{}}
	}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

// seed inserts an object directly, bypassing the mutation log, and returns its
// id. origin is "SYSTEM_DEFINED" or "USER_DEFINED".
func (f *fakeConsole) seed(coll, origin string, obj map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.insert(coll, origin, obj)
	if coll == collNetworks {
		f.projectMDNSLocked()
	}
	return id
}

func (f *fakeConsole) insert(coll, origin string, obj map[string]any) string {
	f.nextID++
	id := fmt.Sprintf("%s-%03d", strings.NewReplacer("/", "-").Replace(coll), f.nextID)
	stored := map[string]any{}
	for k, v := range obj {
		stored[k] = v
	}
	stored["id"] = id
	stored["metadata"] = map[string]any{"origin": origin}
	if coll == collPolicies {
		// Every policy carries a server-assigned evaluation index.
		stored["index"] = 10000 + f.nextID
		stored["metadata"] = map[string]any{"origin": origin, "configurable": origin == originSystem}
	}
	c := f.coll[coll]
	c.order = append(c.order, id)
	c.byID[id] = stored
	return id
}

func (f *fakeConsole) get(coll, id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.coll[coll].byID[id]
}

// objectNamed returns the object in coll whose "name" matches, or nil.
func (f *fakeConsole) objectNamed(coll, name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, obj := range f.coll[coll].byID {
		if obj["name"] == name {
			return obj
		}
	}
	return nil
}

// policyNamed returns the policy with this name and zone pair, or nil.
func (f *fakeConsole) policyNamed(name, srcZoneID, dstZoneID string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, obj := range f.coll[collPolicies].byID {
		if obj["name"] == name && endpointZone(obj, "source") == srcZoneID && endpointZone(obj, "destination") == dstZoneID {
			return obj
		}
	}
	return nil
}

// policyOrder lists the policy names in evaluation order.
func (f *fakeConsole) policyOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, obj := range f.coll[collPolicies].list() {
		out = append(out, obj["name"].(string))
	}
	return out
}

func (f *fakeConsole) names(coll string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, obj := range f.coll[coll].list() {
		if n, ok := obj["name"].(string); ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeConsole) recorded() []mutation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mutation(nil), f.mutations...)
}

// env returns the environment cmd/unifi needs to talk to this fake.
func (f *fakeConsole) env() []string {
	return []string{
		"UNIFI_URL=" + f.URL,
		"UNIFI_API_KEY=" + f.apiKey,
		"UNIFI_SITE=Default",
		"UNIFI_INSECURE_TLS=1",
	}
}

// ------------------------------------------------------------------ routing

func (f *fakeConsole) handle(w http.ResponseWriter, r *http.Request) {
	// Read the body once up front: it is needed both by the mutation log and
	// by the handler, and an http.Request body can only be consumed once.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		f.fail(w, http.StatusBadRequest, "api.invalid-payload", err.Error())
		return
	}

	if r.Header.Get("X-API-KEY") != f.apiKey {
		// Auth failures use a different envelope from application errors.
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": map[string]any{"code": 401, "message": "Unauthorized"}})
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, legacyPrefix+"default/"); ok {
		f.handleLegacy(w, r, raw, rest)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, apiPrefix)
	if !ok {
		f.fail(w, http.StatusNotFound, "api.not-found", "no such path")
		return
	}

	switch {
	case path == "/info":
		writeJSON(w, map[string]any{"applicationVersion": "10.6.101"})
		return
	case path == "/sites":
		f.writePage(w, r, []map[string]any{{"id": f.siteID, "internalReference": "default", "name": "Default"}})
		return
	}

	rest, ok := strings.CutPrefix(path, "/sites/"+f.siteID+"/")
	if !ok {
		f.fail(w, http.StatusNotFound, "api.not-found", "no such site")
		return
	}

	coll, id := splitCollection(rest)
	if coll == "" {
		f.fail(w, http.StatusNotFound, "api.not-found", "no such collection: "+rest)
		return
	}
	if coll == collZones || coll == collPolicies {
		if fault := f.firewallFault; fault != nil {
			f.fail(w, fault.status, fault.code, fault.message)
			return
		}
		if !f.zbfConfigured {
			f.fail(w, http.StatusBadRequest, codeZBFNotConfigured, "Zone Based Firewall is not configured")
			return
		}
	}
	if coll == collPolicies && id == "ordering" {
		f.handleOrdering(w, r, raw)
		return
	}
	f.handleCollection(w, r, raw, coll, id)
}

// splitCollection maps the path under /sites/{id}/ onto a collection name and
// an optional object id. Collection names contain a slash themselves, so a
// plain Cut on "/" will not do.
func splitCollection(rest string) (coll, id string) {
	for _, name := range []string{collWiFi, collDNS, collZones, collPolicies, collNetworks} {
		if rest == name {
			return name, ""
		}
		if tail, ok := strings.CutPrefix(rest, name+"/"); ok && !strings.Contains(tail, "/") {
			return name, tail
		}
	}
	return "", ""
}

func (f *fakeConsole) handleCollection(w http.ResponseWriter, r *http.Request, raw []byte, coll, id string) {
	if r.Method != http.MethodGet {
		f.record(r, raw, coll, id)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.coll[coll]
	if coll == collNetworks && r.Method != http.MethodGet {
		// Runs before the unlock above: a network write moves the legacy
		// mdns projection, as on a real console.
		defer f.projectMDNSLocked()
	}

	switch {
	case r.Method == http.MethodGet && id == "":
		items := make([]map[string]any, 0, len(c.order))
		for _, obj := range c.list() {
			items = append(items, overview(coll, f.visible(coll, obj)))
		}
		f.writePageLocked(w, r, items)

	case r.Method == http.MethodGet:
		obj, ok := c.byID[id]
		if !ok {
			f.fail(w, http.StatusNotFound, "api.not-found", "no such object")
			return
		}
		writeJSON(w, f.visible(coll, obj))

	case r.Method == http.MethodPost:
		body, err := decodeBody(raw)
		if err != nil {
			f.fail(w, http.StatusBadRequest, "api.invalid-payload", err.Error())
			return
		}
		if coll == collDNS {
			if _, ok := body["ttlSeconds"]; !ok {
				body["ttlSeconds"] = float64(0)
			}
		}
		newID := f.insert(coll, originUser, body)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, c.byID[newID])

	case r.Method == http.MethodPut && coll == collWiFi && f.wifiPutFault:
		// A validation error that quotes the offending payload — which for a
		// wifi update includes the passphrase.
		f.fail(w, http.StatusBadRequest, "api.invalid-payload", "rejected payload: "+string(raw))

	case r.Method == http.MethodPut:
		obj, ok := c.byID[id]
		if !ok {
			f.fail(w, http.StatusNotFound, "api.not-found", "no such object")
			return
		}
		body, err := decodeBody(raw)
		if err != nil {
			f.fail(w, http.StatusBadRequest, "api.invalid-payload", err.Error())
			return
		}
		// A PUT replaces the writable fields; id and metadata stay server-owned.
		updated := map[string]any{"id": obj["id"], "metadata": obj["metadata"]}
		for k, v := range body {
			updated[k] = v
		}
		c.byID[id] = updated
		writeJSON(w, updated)

	case r.Method == http.MethodDelete:
		if _, ok := c.byID[id]; !ok {
			f.fail(w, http.StatusNotFound, "api.not-found", "no such object")
			return
		}
		delete(c.byID, id)
		c.order = remove(c.order, id)
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "GET, POST")
		f.fail(w, http.StatusMethodNotAllowed, "api.method-not-allowed", r.Method)
	}
}

func (f *fakeConsole) handleOrdering(w http.ResponseWriter, r *http.Request, raw []byte) {
	if r.Method != http.MethodPut {
		f.fail(w, http.StatusMethodNotAllowed, "api.method-not-allowed", r.Method)
		return
	}
	// The console orders policies per zone pair and rejects the call without
	// both zone ids.
	srcZone := r.URL.Query().Get("sourceFirewallZoneId")
	dstZone := r.URL.Query().Get("destinationFirewallZoneId")
	if srcZone == "" || dstZone == "" {
		f.fail(w, http.StatusBadRequest, "api.request.error",
			"Required request parameter 'sourceFirewallZoneId' for method parameter type UUID is not present")
		return
	}
	f.record(r, raw, collPolicies, "ordering")

	body, err := decodeBody(raw)
	if err != nil {
		f.fail(w, http.StatusBadRequest, "api.invalid-payload", err.Error())
		return
	}
	ids, _ := body["orderedFirewallPolicyIds"].(map[string]any)
	before, _ := ids["beforeSystemDefined"].([]any)

	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.coll[collPolicies]
	var reordered []string
	for _, v := range before {
		id, _ := v.(string)
		obj, ok := c.byID[id]
		if !ok {
			f.fail(w, http.StatusBadRequest, "api.invalid-payload", "unknown policy id")
			return
		}
		if endpointZone(obj, "source") != srcZone || endpointZone(obj, "destination") != dstZone {
			f.fail(w, http.StatusBadRequest, "api.invalid-payload", "policy is not in this zone pair")
			return
		}
		reordered = append(reordered, id)
	}
	// The reordered policies take the slots their zone pair already occupied,
	// leaving every other policy where it was.
	inPair := map[string]bool{}
	for _, id := range reordered {
		inPair[id] = true
	}
	var slots []int
	for i, id := range c.order {
		obj := c.byID[id]
		if endpointZone(obj, "source") == srcZone && endpointZone(obj, "destination") == dstZone {
			slots = append(slots, i)
			inPair[id] = true
		}
	}
	for _, id := range c.order {
		if inPair[id] && !contains(reordered, id) {
			reordered = append(reordered, id)
		}
	}
	next := append([]string(nil), c.order...)
	for i, slot := range slots {
		if i < len(reordered) {
			next[slot] = reordered[i]
		}
	}
	c.order = next
	w.WriteHeader(http.StatusNoContent)
}

func endpointZone(obj map[string]any, end string) string {
	ep, _ := obj[end].(map[string]any)
	id, _ := ep["zoneId"].(string)
	return id
}

// visible applies the response-shaping quirks of a real console that are not
// about the overview/detail split.
func (f *fakeConsole) visible(coll string, obj map[string]any) map[string]any {
	if !f.omitUserPolicyIDs || coll != collPolicies {
		return obj
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta["origin"] == originSystem {
		return obj
	}
	out := map[string]any{}
	for k, v := range obj {
		if k != "id" {
			out[k] = v
		}
	}
	return out
}

// overview strips the fields the real API omits from list responses, so the
// tool is exercised against the same overview/detail split a console has.
func overview(coll string, obj map[string]any) map[string]any {
	keep := map[string][]string{
		collNetworks: {"id", "name", "management", "enabled", "vlanId", "default", "metadata"},
		collWiFi:     {"id", "type", "name", "enabled", "metadata", "network", "securityConfiguration", "broadcastingFrequenciesGHz"},
	}[coll]
	if keep == nil {
		return obj
	}
	out := map[string]any{}
	for _, k := range keep {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	// The wifi overview carries the security type but not the passphrase.
	if sec, ok := out["securityConfiguration"].(map[string]any); ok {
		out["securityConfiguration"] = map[string]any{"type": sec["type"]}
	}
	return out
}

func (f *fakeConsole) record(r *http.Request, raw []byte, coll, id string) {
	body, _ := decodeBody(raw)
	path := coll
	if id != "" {
		path = coll + "/" + id
	}
	m := mutation{Method: r.Method, Path: path, Body: body}
	if f.onMutation != nil {
		f.onMutation(m)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutations = append(f.mutations, m)
}

// ------------------------------------------------------------ legacy API

// legacyNetworkID is the `rest/networkconf` id of the network with this name.
func legacyNetworkID(integrationID string) string { return "legacy-" + integrationID }

func (f *fakeConsole) legacyNetworkIDNamed(name string) string {
	return legacyNetworkID(f.objectNamed(collNetworks, name)["id"].(string))
}

// seedUser inserts a `rest/user` entry directly and returns its `_id`.
func (f *fakeConsole) seedUser(obj map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insertUser(obj)
}

func (f *fakeConsole) insertUser(obj map[string]any) string {
	f.nextID++
	id := fmt.Sprintf("user-%03d", f.nextID)
	stored := map[string]any{"_id": id}
	for k, v := range obj {
		stored[k] = v
	}
	f.users[id] = stored
	f.userOrder = append(f.userOrder, id)
	return id
}

func (f *fakeConsole) user(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[id]
}

func (f *fakeConsole) userByMAC(mac string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u["mac"] == mac {
			return u
		}
	}
	return nil
}

func (f *fakeConsole) legacyRequestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.legacyRequests...)
}

// handleLegacy serves the legacy controller API under /api/s/default/. Every
// response uses the {"meta":{"rc"},"data":[…]} envelope. A PUT merges into
// the stored entry rather than replacing it, which is what a live console
// was seen to do: `PUT {"use_fixedip":false}` keeps fixed_ip and network_id.
func (f *fakeConsole) handleLegacy(w http.ResponseWriter, r *http.Request, raw []byte, rest string) {
	f.mu.Lock()
	f.legacyRequests = append(f.legacyRequests, r.Method+" "+rest)
	f.mu.Unlock()
	if f.onLegacyRequest != nil {
		f.onLegacyRequest(r.Method, rest)
	}
	if r.Method != http.MethodGet {
		f.record(r, raw, "legacy/"+rest, "")
	}
	body, err := decodeBody(raw)
	if err != nil {
		legacyFail(w, http.StatusBadRequest, "api.err.InvalidPayload")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(data []map[string]any) {
		writeJSON(w, map[string]any{"meta": map[string]any{"rc": "ok"}, "data": data})
	}

	switch id, isItem := strings.CutPrefix(rest, "rest/user/"); {
	case rest == "rest/user" && r.Method == http.MethodGet:
		data := []map[string]any{}
		for _, id := range f.userOrder {
			data = append(data, f.users[id])
		}
		ok(data)

	case rest == "rest/user" && r.Method == http.MethodPost:
		mac, _ := body["mac"].(string)
		if mac == "" {
			legacyFail(w, http.StatusBadRequest, "api.err.InvalidMac")
			return
		}
		for _, u := range f.users {
			if u["mac"] == mac {
				legacyFail(w, http.StatusBadRequest, "api.err.MacUsed")
				return
			}
		}
		ok([]map[string]any{f.users[f.insertUser(body)]})

	case isItem && (r.Method == http.MethodGet || r.Method == http.MethodPut):
		u, found := f.users[id]
		if !found {
			legacyFail(w, http.StatusNotFound, "api.err.NotFound")
			return
		}
		for k, v := range body {
			u[k] = v
		}
		ok([]map[string]any{u})

	case rest == "rest/setting" && r.Method == http.MethodGet:
		ok(f.settings)

	case rest == "set/setting/mdns" && r.Method == http.MethodPost:
		if msg := checkMDNSWrite(body); msg != "" || f.mdnsWriteRefused {
			if msg == "" {
				msg = "api.err.InvalidValue"
			}
			legacyFail(w, http.StatusBadRequest, msg)
			return
		}
		// enabled_for and enabled_for_network_ids are a projection of the
		// networks' flags: what the body says of them is ignored.
		m := f.mdnsLocked()
		for _, k := range []string{"mode", "predefined_services", "custom_services"} {
			m[k] = body[k]
		}
		ok([]map[string]any{m})

	case rest == "rest/networkconf" && r.Method == http.MethodGet:
		data := []map[string]any{}
		for _, n := range f.coll[collNetworks].list() {
			data = append(data, map[string]any{"_id": legacyNetworkID(n["id"].(string)), "name": n["name"],
				"mdns_enabled": n["mdnsForwardingEnabled"] == true})
		}
		ok(data)

	case rest == "stat/sta" && r.Method == http.MethodGet:
		data := []map[string]any{}
		for mac, net := range f.stations {
			data = append(data, map[string]any{"mac": mac, "network_id": net})
		}
		ok(data)

	case rest == "cmd/stamgr" && r.Method == http.MethodPost:
		if body["cmd"] == "forget-sta" {
			macs, _ := body["macs"].([]any)
			for _, m := range macs {
				for id, u := range f.users {
					if u["mac"] == m {
						delete(f.users, id)
						f.userOrder = remove(f.userOrder, id)
					}
				}
			}
		}
		ok([]map[string]any{})

	default:
		legacyFail(w, http.StatusNotFound, "api.err.NotFound")
	}
}

const (
	fakeLegacySiteID = "5f0000000000000000000001"
	fakeMDNSID       = "5f00000000000000000000d5"
)

// mdnsCatalogue is `predefined_services` as a console that has not been
// written since its migration to UniFi Network 10.6 reports it, in the legacy
// mode "auto": every service the console knows.
var mdnsCatalogue = []string{
	"amazon_devices", "android_tv_remote", "apple_airDrop", "apple_airPlay", "apple_file_sharing",
	"apple_iChat", "apple_iTunes", "aqara", "bose", "dns_service_discovery", "ftp_servers",
	"google_chromecast", "homeKit", "matter_network", "philips_hue", "printers", "roku",
	"scanners", "sonos", "spotify_connect", "ssh_servers", "time_capsule", "web_servers",
	"windows_file_sharing_samba",
}

// defaultMDNSSetting is the mdns `rest/setting` entry as the console UI's
// Auto leaves it. enabled_for and enabled_for_network_ids are filled in by
// projectMDNSLocked.
func defaultMDNSSetting() map[string]any {
	return map[string]any{
		"_id": fakeMDNSID, "key": "mdns", "site_id": fakeLegacySiteID,
		"mode": "all", "enabled_for": "none", "enabled_for_network_ids": []any{},
		"predefined_services": []any{}, "custom_services": []any{},
	}
}

// checkMDNSWrite is what the console checks of a set/setting/mdns body: the
// api.err.* message it refuses it with, or "".
func checkMDNSWrite(body map[string]any) string {
	for _, k := range []string{"mode", "predefined_services", "custom_services", "enabled_for", "enabled_for_network_ids"} {
		if _, ok := body[k]; !ok {
			return "api.err.InvalidPayload"
		}
	}
	switch body["mode"] {
	case "all", "auto", "custom":
	default:
		return "api.err.InvalidPayload"
	}
	custom, ok := body["custom_services"].([]any)
	if !ok {
		return "api.err.InvalidPayload"
	}
	for _, e := range custom {
		cs, _ := e.(map[string]any)
		if len(cs) != 2 || cs["address"] == "" || cs["name"] == "" || cs["address"] == nil || cs["name"] == nil {
			return "api.err.InvalidPayload"
		}
	}
	return ""
}

// projectMDNSLocked derives the mdns entry's enabled_for and
// enabled_for_network_ids from the networks' mdnsForwardingEnabled, as the
// console does: all true is "all", none true is "none", otherwise "some"
// with the true ones listed.
func (f *fakeConsole) projectMDNSLocked() {
	nets := f.coll[collNetworks].list()
	ids := []any{}
	for _, n := range nets {
		if n["mdnsForwardingEnabled"] == true {
			ids = append(ids, legacyNetworkID(n["id"].(string)))
		}
	}
	enabledFor := "some"
	switch len(ids) {
	case 0:
		enabledFor = "none"
	case len(nets):
		enabledFor = "all"
	}
	m := f.mdnsLocked()
	m["enabled_for"], m["enabled_for_network_ids"] = enabledFor, ids
}

// serviceCodes is a predefined_services list.
func serviceCodes(codes ...string) []any {
	out := []any{}
	for _, c := range codes {
		out = append(out, map[string]any{"code": c})
	}
	return out
}

func (f *fakeConsole) mdnsLocked() map[string]any {
	for _, s := range f.settings {
		if s["key"] == "mdns" {
			return s
		}
	}
	return nil
}

// mdnsSetting returns a copy of the mdns `rest/setting` entry.
func (f *fakeConsole) mdnsSetting() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{}
	for k, v := range f.mdnsLocked() {
		out[k] = v
	}
	return out
}

// setMDNS merges fields into the mdns `rest/setting` entry, bypassing the
// mutation log.
func (f *fakeConsole) setMDNS(fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.mdnsLocked()
	for k, v := range fields {
		m[k] = v
	}
}

func legacyFail(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"meta": map[string]any{"rc": "error", "msg": msg}, "data": []any{}})
}

// ------------------------------------------------------------------ helpers

func (f *fakeConsole) writePage(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writePageLocked(w, r, items)
}

// writePageLocked applies offset/limit the way the console does, including the
// server-side cap of 200 that forces clients to page.
func (f *fakeConsole) writePageLocked(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	limit := 25
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, pageLimit)
	}
	offset := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	total := len(items)
	page := []map[string]any{}
	if offset < total {
		page = items[offset:min(offset+limit, total)]
	}
	writeJSON(w, map[string]any{
		"offset": offset, "limit": limit,
		"count": len(page), "totalCount": total, "data": page,
	})
}

func (f *fakeConsole) fail(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{
		"statusCode": status, "statusName": http.StatusText(status),
		"code": code, "message": message,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func decodeBody(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	return body, nil
}

func remove(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

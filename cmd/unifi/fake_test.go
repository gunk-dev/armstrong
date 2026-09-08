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

	mu   sync.Mutex
	coll map[string]*collection
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
	return f.insert(coll, origin, obj)
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

	switch {
	case r.Method == http.MethodGet && id == "":
		items := make([]map[string]any, 0, len(c.order))
		for _, obj := range c.list() {
			items = append(items, overview(coll, obj))
		}
		f.writePageLocked(w, r, items)

	case r.Method == http.MethodGet:
		obj, ok := c.byID[id]
		if !ok {
			f.fail(w, http.StatusNotFound, "api.not-found", "no such object")
			return
		}
		writeJSON(w, obj)

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
		if _, ok := c.byID[id]; !ok {
			f.fail(w, http.StatusBadRequest, "api.invalid-payload", "unknown policy id")
			return
		}
		reordered = append(reordered, id)
	}
	for _, id := range c.order {
		if !contains(reordered, id) {
			reordered = append(reordered, id)
		}
	}
	c.order = reordered
	w.WriteHeader(http.StatusNoContent)
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutations = append(f.mutations, mutation{Method: r.Method, Path: path, Body: body})
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

// Package porkbunfake is a stand-in for the Porkbun DNS HTTP API, shared by
// the tests that drive the dns command end to end.
package porkbunfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Record is a DNS record as the Porkbun API reports it.
type Record struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	TTL      string `json:"ttl"`
	Priority string `json:"prio"`
}

// API serves the records it is seeded with, per domain, to requests carrying
// its key pair, and logs every mutating request without applying it.
type API struct {
	apiKey, secretKey string

	mu       sync.Mutex
	records  map[string][]Record
	calls    []string
	requests int
	failOn   string
}

// FailOn makes every mutating request whose logged call starts with prefix
// fail, after it is logged.
func (a *API) FailOn(prefix string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failOn = prefix
}

// New returns an API that accepts apiKey and secretKey.
func New(apiKey, secretKey string, records map[string][]Record) *API {
	return &API{apiKey: apiKey, secretKey: secretKey, records: records}
}

// Calls lists the mutating requests in order, e.g. "create gunk.dev A www 1.2.3.4".
func (a *API) Calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// Requests counts every request, reads included, so a test can assert the
// API was never contacted.
func (a *API) Requests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests++

	// Porkbun authenticates with credentials in the JSON body, not headers.
	body, _ := io.ReadAll(r.Body)
	var req struct {
		APIKey       string `json:"apikey"`
		SecretAPIKey string `json:"secretapikey"`
		Type         string `json:"type"`
		Name         string `json:"name"`
		Content      string `json:"content"`
	}
	json.Unmarshal(body, &req)
	if req.APIKey != a.apiKey || req.SecretAPIKey != a.secretKey {
		http.Error(w, `{"status":"ERROR","message":"bad credentials"}`, http.StatusForbidden)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/json/v3/dns/"), "/")
	if len(parts) < 2 {
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		return
	}
	action, domain := parts[0], parts[1]
	switch {
	case action == "retrieve":
		json.NewEncoder(w).Encode(map[string]any{"status": "SUCCESS", "records": a.records[domain]})
		return
	case action == "create":
		a.calls = append(a.calls, fmt.Sprintf("create %s %s %s %s", domain, req.Type, displayName(req.Name), req.Content))
	case action == "editByNameType" && len(parts) == 4:
		a.calls = append(a.calls, fmt.Sprintf("edit %s %s %s", domain, parts[2], displayName(parts[3])))
	case action == "delete" && len(parts) == 3:
		a.calls = append(a.calls, fmt.Sprintf("delete %s id=%s", domain, parts[2]))
	default:
		http.Error(w, "unexpected action "+action, http.StatusNotFound)
		return
	}
	if a.failOn != "" && strings.HasPrefix(a.calls[len(a.calls)-1], a.failOn) {
		fmt.Fprint(w, `{"status":"ERROR","message":"injected failure"}`)
		return
	}
	fmt.Fprint(w, `{"status":"SUCCESS"}`)
}

func displayName(name string) string {
	if name == "" {
		return "@"
	}
	return name
}

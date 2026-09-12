package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// fakePorkbunAPI is a stand-in for the Porkbun HTTP API. It serves the records
// it is seeded with per domain and records every mutating request, so a test
// can drive the real cobra command end to end. requests counts every request,
// mutating or not, so a test can assert the API was never contacted at all.
type fakePorkbunAPI struct {
	records  map[string][]porkbunRecord
	calls    []string
	requests int
}

func (f *fakePorkbunAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Porkbun authenticates with credentials in the JSON body, not headers.
	f.requests++

	var auth authBody
	body, _ := readAllAndDecode(r, &auth)
	if auth.APIKey != "test-key" || auth.SecretAPIKey != "test-secret" {
		http.Error(w, `{"status":"ERROR","message":"bad credentials"}`, http.StatusForbidden)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/json/v3/dns/")
	parts := strings.Split(path, "/")
	action, domain := parts[0], parts[1]

	switch action {
	case "retrieve":
		json.NewEncoder(w).Encode(retrieveResponse{Status: "SUCCESS", Records: f.records[domain]})
		return
	case "create":
		var req createRequest
		json.Unmarshal(body, &req)
		f.calls = append(f.calls, fmt.Sprintf("create %s %s %s %s", domain, req.Type, displayName(req.Name), req.Content))
	case "editByNameType":
		f.calls = append(f.calls, fmt.Sprintf("edit %s %s %s", domain, parts[2], displayName(parts[3])))
	case "delete":
		f.calls = append(f.calls, fmt.Sprintf("delete %s id=%s", domain, parts[2]))
	default:
		http.Error(w, "unexpected action "+action, http.StatusNotFound)
		return
	}
	fmt.Fprint(w, `{"status":"SUCCESS"}`)
}

func readAllAndDecode(r *http.Request, v any) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), json.Unmarshal(buf.Bytes(), v)
}

// runSyncCmd runs the real `dns sync` cobra command against srv with stdin set
// to input, and returns its stdout.
func runSyncCmd(t *testing.T, srv *httptest.Server, input string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("PORKBUN_API_KEY", "test-key")
	t.Setenv("PORKBUN_SECRET_KEY", "test-secret")
	t.Setenv("PORKBUN_API_BASE", srv.URL+"/api/json/v3")

	var out bytes.Buffer
	cmd := newSyncCmd()
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSyncCmd_TwoZones_EndToEnd(t *testing.T) {
	api := &fakePorkbunAPI{records: map[string][]porkbunRecord{
		"gunk.dev": {
			// matches the desired record exactly -> no call
			{ID: "1", Name: "www.gunk.dev", Type: "A", Content: "1.2.3.4", TTL: "600"},
			// unmatched -> pruned
			{ID: "2", Name: "stale.gunk.dev", Type: "A", Content: "9.9.9.9", TTL: "600"},
			// never pruned
			{ID: "3", Name: "preview-7.app.gunk.dev", Type: "CNAME", Content: "app.fly.dev", TTL: "600"},
			{ID: "4", Name: "gunk.dev", Type: "NS", Content: "ns1.porkbun.com", TTL: "86400"},
		},
		"broken.dev": {},
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	out, err := runSyncCmd(t, srv, twoZoneInput, "--prune")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}

	want := []string{
		"delete gunk.dev id=2",
		"create broken.dev A @ 5.6.7.8",
	}
	if !slices.Equal(api.calls, want) {
		t.Errorf("API calls = %v; want %v", api.calls, want)
	}
	for _, line := range []string{"== gunk.dev ==", "OK     A www", "DELETE A stale", "== broken.dev ==", "CREATE A @ -> 5.6.7.8"} {
		if !strings.Contains(out, line) {
			t.Errorf("output missing %q; got:\n%s", line, out)
		}
	}
}

func TestSyncCmd_SingleZoneDryRun_EndToEnd(t *testing.T) {
	api := &fakePorkbunAPI{records: map[string][]porkbunRecord{
		"gunk.dev": {{ID: "1", Name: "www.gunk.dev", Type: "A", Content: "1.2.3.4", TTL: "60"}},
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	input := `{"domain": "gunk.dev", "records": [{"type": "A", "name": "www", "content": "1.2.3.4", "ttl": 600}]}`
	out, err := runSyncCmd(t, srv, input, "--prune", "--dry-run")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}

	if len(api.calls) != 0 {
		t.Errorf("dry-run made mutating API calls: %v", api.calls)
	}
	if !strings.Contains(out, "UPDATE A www") {
		t.Errorf("output missing planned update; got:\n%s", out)
	}
}

func TestSyncCmd_BadInput_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(&fakePorkbunAPI{})
	defer srv.Close()

	if _, err := runSyncCmd(t, srv, `[]`); err == nil {
		t.Error("empty array: got nil error, want failure")
	}
	if _, err := runSyncCmd(t, srv, `"gunk.dev"`); err == nil {
		t.Error("scalar input: got nil error, want failure")
	}
}

// TestSyncCmd_DuplicateZones_EndToEnd covers the copied-directory mistake: two
// entries naming the same domain, each declaring only half the records. With
// --prune each pass would delete the other's records, so the command must fail
// before it touches the API at all.
func TestSyncCmd_DuplicateZones_EndToEnd(t *testing.T) {
	tests := []struct {
		name         string
		firstDomain  string
		secondDomain string
		wantErr      string
	}{
		{"identical", "gunk.dev", "gunk.dev", `duplicate zone "gunk.dev" in input`},
		{"case and trailing dot", "Gunk.dev", "gunk.dev.", `duplicate zone "gunk.dev" in input`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakePorkbunAPI{records: map[string][]porkbunRecord{
				"gunk.dev": {
					{ID: "1", Name: "www.gunk.dev", Type: "A", Content: "1.2.3.4", TTL: "600"},
					{ID: "2", Name: "mail.gunk.dev", Type: "A", Content: "5.6.7.8", TTL: "600"},
				},
			}}
			srv := httptest.NewServer(api)
			defer srv.Close()

			input := fmt.Sprintf(`[
  {"domain": %q, "records": [{"type": "A", "name": "www",  "content": "1.2.3.4", "ttl": 600}]},
  {"domain": %q, "records": [{"type": "A", "name": "mail", "content": "5.6.7.8", "ttl": 600}]}
]`, tt.firstDomain, tt.secondDomain)

			out, err := runSyncCmd(t, srv, input, "--prune")
			if err == nil {
				t.Fatalf("got nil error, want duplicate-zone failure; output:\n%s", out)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v; want it to mention %s", err, tt.wantErr)
			}
			if api.requests != 0 || len(api.calls) != 0 {
				t.Errorf("contacted the API despite invalid input: %d requests, calls %v", api.requests, api.calls)
			}
		})
	}
}

func TestSyncCmd_EmptyDomain_EndToEnd(t *testing.T) {
	api := &fakePorkbunAPI{records: map[string][]porkbunRecord{}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	for _, input := range []string{
		`{"domain": "", "records": [{"type": "A", "name": "www", "content": "1.2.3.4", "ttl": 600}]}`,
		`[{"domain": "gunk.dev", "records": []}, {"domain": "  ", "records": []}]`,
	} {
		out, err := runSyncCmd(t, srv, input, "--prune")
		if err == nil {
			t.Fatalf("got nil error for %s, want empty-domain failure; output:\n%s", input, out)
		}
		if !strings.Contains(err.Error(), "empty domain") {
			t.Errorf("error = %v; want it to mention an empty domain", err)
		}
		if api.requests != 0 || len(api.calls) != 0 {
			t.Errorf("contacted the API despite empty domain: %d requests, calls %v", api.requests, api.calls)
		}
	}
}

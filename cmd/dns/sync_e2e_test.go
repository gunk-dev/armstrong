package main

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gunk.dev/armstrong/internal/porkbunfake"
)

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
	api := porkbunfake.New("test-key", "test-secret", map[string][]porkbunfake.Record{
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
	})
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
	if !slices.Equal(api.Calls(), want) {
		t.Errorf("API calls = %v; want %v", api.Calls(), want)
	}
	for _, line := range []string{"== gunk.dev ==", "OK     A www", "DELETE A stale", "== broken.dev ==", "CREATE A @ -> 5.6.7.8"} {
		if !strings.Contains(out, line) {
			t.Errorf("output missing %q; got:\n%s", line, out)
		}
	}
}

func TestSyncCmd_SingleZoneDryRun_EndToEnd(t *testing.T) {
	api := porkbunfake.New("test-key", "test-secret", map[string][]porkbunfake.Record{
		"gunk.dev": {{ID: "1", Name: "www.gunk.dev", Type: "A", Content: "1.2.3.4", TTL: "60"}},
	})
	srv := httptest.NewServer(api)
	defer srv.Close()

	input := `{"domain": "gunk.dev", "records": [{"type": "A", "name": "www", "content": "1.2.3.4", "ttl": 600}]}`
	out, err := runSyncCmd(t, srv, input, "--prune", "--dry-run")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}

	if len(api.Calls()) != 0 {
		t.Errorf("dry-run made mutating API calls: %v", api.Calls())
	}
	if !strings.Contains(out, "UPDATE A www") {
		t.Errorf("output missing planned update; got:\n%s", out)
	}
}

func TestSyncCmd_BadInput_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(porkbunfake.New("test-key", "test-secret", nil))
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
			api := porkbunfake.New("test-key", "test-secret", map[string][]porkbunfake.Record{
				"gunk.dev": {
					{ID: "1", Name: "www.gunk.dev", Type: "A", Content: "1.2.3.4", TTL: "600"},
					{ID: "2", Name: "mail.gunk.dev", Type: "A", Content: "5.6.7.8", TTL: "600"},
				},
			})
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
			if api.Requests() != 0 || len(api.Calls()) != 0 {
				t.Errorf("contacted the API despite invalid input: %d requests, calls %v", api.Requests(), api.Calls())
			}
		})
	}
}

func TestSyncCmd_EmptyDomain_EndToEnd(t *testing.T) {
	api := porkbunfake.New("test-key", "test-secret", nil)
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
		if api.Requests() != 0 || len(api.Calls()) != 0 {
			t.Errorf("contacted the API despite empty domain: %d requests, calls %v", api.Requests(), api.Calls())
		}
	}
}

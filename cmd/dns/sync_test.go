package main

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestNormalizeContent(t *testing.T) {
	tests := []struct {
		recordType string
		content    string
		want       string
	}{
		// AAAA records: different representations of the same IPv6 address should normalize
		{"AAAA", "2a09:8280:1::f8:c96e:0", "2a09:8280:1::f8:c96e:0"},
		{"AAAA", "2a09:8280:0001:0000:0000:00f8:c96e:0000", "2a09:8280:1::f8:c96e:0"},
		{"AAAA", "2a09:8280:1:0:0:f8:c96e:0", "2a09:8280:1::f8:c96e:0"},
		{"AAAA", "::1", "::1"},

		// A records: passed through unchanged
		{"A", "66.241.125.235", "66.241.125.235"},

		// Other types: passed through unchanged
		{"CNAME", "example.com", "example.com"},
		{"MX", "mail.example.com", "mail.example.com"},

		// Invalid IPv6: passed through unchanged
		{"AAAA", "not-an-ip", "not-an-ip"},
	}

	for _, tt := range tests {
		got := normalizeContent(tt.recordType, tt.content)
		if got != tt.want {
			t.Errorf("normalizeContent(%q, %q) = %q, want %q", tt.recordType, tt.content, got, tt.want)
		}
	}
}

// fakePorkbun implements porkbunMutator and records every call so tests can
// assert that --dry-run makes no mutations, and that multi-zone syncs address
// the right domain.
type fakePorkbun struct {
	existing []porkbunRecord
	// byDomain, when it has an entry for a domain, takes precedence over
	// existing so multi-zone tests can give each zone its own records.
	byDomain map[string][]porkbunRecord

	retrieves []string
	creates   []createRequest
	edits     []editRequest
	deletes   []string
	// domains records the domain of every mutating call, in order.
	domains []string
}

func (f *fakePorkbun) retrieve(domain string) ([]porkbunRecord, error) {
	f.retrieves = append(f.retrieves, domain)
	if recs, ok := f.byDomain[domain]; ok {
		return recs, nil
	}
	return f.existing, nil
}

func (f *fakePorkbun) create(domain string, req createRequest) error {
	f.creates = append(f.creates, req)
	f.domains = append(f.domains, domain)
	return nil
}

func (f *fakePorkbun) editByNameType(domain, recordType, subdomain string, req editRequest) error {
	f.edits = append(f.edits, req)
	f.domains = append(f.domains, domain)
	return nil
}

func (f *fakePorkbun) deleteByID(domain, id string) error {
	f.deletes = append(f.deletes, id)
	f.domains = append(f.domains, domain)
	return nil
}

func (f *fakePorkbun) mutationCount() int {
	return len(f.creates) + len(f.edits) + len(f.deletes)
}

// fixtureInput returns a dnsInput + existing records that together require one
// of each mutation kind (create, update, delete) so tests can check that
// dry-run still prints all of them but executes none.
func fixtureInput() (dnsInput, []porkbunRecord) {
	input := dnsInput{
		Domain: "example.com",
		Records: []dnsRecord{
			// existing A with stale TTL -> UPDATE
			{Type: "A", Name: "www", Content: "1.2.3.4", TTL: 600},
			// not-yet-existing AAAA -> CREATE
			{Type: "AAAA", Name: "ipv6", Content: "::1", TTL: 300},
		},
	}
	existing := []porkbunRecord{
		// matches www A but with a different TTL -> UPDATE
		{ID: "1", Name: "www.example.com", Type: "A", Content: "1.2.3.4", TTL: "60"},
		// unmatched, prunable -> DELETE when --prune
		{ID: "2", Name: "stale.example.com", Type: "A", Content: "9.9.9.9", TTL: "300"},
		// NS record should never be pruned
		{ID: "3", Name: "example.com", Type: "NS", Content: "ns1.porkbun.com", TTL: "86400"},
	}
	return input, existing
}

func TestSyncRecords_DryRunWithPrune_MakesNoMutations(t *testing.T) {
	input, existing := fixtureInput()
	fake := &fakePorkbun{existing: existing}
	var out bytes.Buffer

	if err := syncRecords(fake, input, true /*prune*/, true /*dryRun*/, &out); err != nil {
		t.Fatalf("syncRecords: %v", err)
	}

	if got := fake.mutationCount(); got != 0 {
		t.Fatalf("dry-run performed %d mutations (creates=%v, edits=%v, deletes=%v); want 0",
			got, fake.creates, fake.edits, fake.deletes)
	}

	output := out.String()
	for _, want := range []string{
		"DRY RUN",
		"UPDATE A www",
		"CREATE AAAA ipv6",
		"DELETE A stale",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q; got:\n%s", want, output)
		}
	}
}

func TestSyncRecords_DryRunWithoutPrune_MakesNoMutations(t *testing.T) {
	input, existing := fixtureInput()
	fake := &fakePorkbun{existing: existing}
	var out bytes.Buffer

	if err := syncRecords(fake, input, false /*prune*/, true /*dryRun*/, &out); err != nil {
		t.Fatalf("syncRecords: %v", err)
	}

	if got := fake.mutationCount(); got != 0 {
		t.Fatalf("dry-run performed %d mutations (creates=%v, edits=%v, deletes=%v); want 0",
			got, fake.creates, fake.edits, fake.deletes)
	}

	output := out.String()
	if !strings.Contains(output, "DRY RUN") {
		t.Errorf("dry-run output missing header; got:\n%s", output)
	}
	// Without --prune, DELETE lines must not appear.
	if strings.Contains(output, "DELETE") {
		t.Errorf("dry-run without --prune should not print DELETE lines; got:\n%s", output)
	}
}

func TestSyncRecords_RealRunWithPrune_PerformsMutations(t *testing.T) {
	input, existing := fixtureInput()
	fake := &fakePorkbun{existing: existing}
	var out bytes.Buffer

	if err := syncRecords(fake, input, true /*prune*/, false /*dryRun*/, &out); err != nil {
		t.Fatalf("syncRecords: %v", err)
	}

	if len(fake.creates) != 1 {
		t.Errorf("creates=%v; want 1", fake.creates)
	}
	if len(fake.edits) != 1 {
		t.Errorf("edits=%v; want 1", fake.edits)
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != "2" {
		t.Errorf("deletes=%v; want [2]", fake.deletes)
	}
	if strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("real run should not print DRY RUN header; got:\n%s", out.String())
	}
}

// twoZoneInput is the JSON a multi-zone `cue export` produces: an array of the
// same per-zone objects the single-zone form uses.
const twoZoneInput = `[
  {"domain": "gunk.dev",   "records": [{"type": "A", "name": "www", "content": "1.2.3.4", "ttl": 600}]},
  {"domain": "broken.dev", "records": [{"type": "A", "name": "", "content": "5.6.7.8", "ttl": 300}]}
]`

func TestParseZones_Array(t *testing.T) {
	zones, err := parseZones([]byte(twoZoneInput))
	if err != nil {
		t.Fatalf("parseZones: %v", err)
	}
	if len(zones) != 2 {
		t.Fatalf("got %d zones; want 2", len(zones))
	}
	if zones[0].Domain != "gunk.dev" || zones[1].Domain != "broken.dev" {
		t.Errorf("zones out of order: %q, %q", zones[0].Domain, zones[1].Domain)
	}
	if len(zones[1].Records) != 1 || zones[1].Records[0].Content != "5.6.7.8" {
		t.Errorf("second zone records = %+v", zones[1].Records)
	}
}

// The single-object form is what gunk.dev's current flat dns/ layout exports;
// it must keep parsing to exactly one zone.
func TestParseZones_SingleObject(t *testing.T) {
	zones, err := parseZones([]byte(`{"domain": "gunk.dev", "records": [{"type": "A", "name": "www", "content": "1.2.3.4", "ttl": 600}]}`))
	if err != nil {
		t.Fatalf("parseZones: %v", err)
	}
	if len(zones) != 1 {
		t.Fatalf("got %d zones; want 1", len(zones))
	}
	if zones[0].Domain != "gunk.dev" || len(zones[0].Records) != 1 {
		t.Errorf("zone = %+v", zones[0])
	}
}

func TestParseZones_BadInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty array", `[]`, "no zones in input"},
		{"empty array with whitespace", "\n  [ ]\n", "no zones in input"},
		{"string", `"gunk.dev"`, "expected a JSON object or array"},
		{"number", `42`, "expected a JSON object or array"},
		{"empty", "   \n", "empty input"},
		{"truncated array", `[{"domain": "gunk.dev"`, "parse input"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zones, err := parseZones([]byte(tt.input))
			if err == nil {
				t.Fatalf("parseZones(%q) = %+v, want error", tt.input, zones)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestSyncZones_TwoZones_UsesEachDomain(t *testing.T) {
	zones, err := parseZones([]byte(twoZoneInput))
	if err != nil {
		t.Fatalf("parseZones: %v", err)
	}
	fake := &fakePorkbun{byDomain: map[string][]porkbunRecord{
		// gunk.dev already has www at the desired TTL -> OK, no mutation.
		"gunk.dev": {{ID: "1", Name: "www.gunk.dev", Type: "A", Content: "1.2.3.4", TTL: "600"}},
		// broken.dev is empty -> the apex A gets created.
		"broken.dev": {},
	}}
	var out bytes.Buffer

	if err := syncZones(fake, zones, true /*prune*/, false /*dryRun*/, &out); err != nil {
		t.Fatalf("syncZones: %v", err)
	}

	if want := []string{"gunk.dev", "broken.dev"}; !slices.Equal(fake.retrieves, want) {
		t.Errorf("retrieves = %v; want %v", fake.retrieves, want)
	}
	if len(fake.creates) != 1 || fake.creates[0].Content != "5.6.7.8" {
		t.Fatalf("creates = %+v; want one apex A for broken.dev", fake.creates)
	}
	if want := []string{"broken.dev"}; !slices.Equal(fake.domains, want) {
		t.Errorf("mutation domains = %v; want %v", fake.domains, want)
	}
	if len(fake.edits) != 0 || len(fake.deletes) != 0 {
		t.Errorf("unexpected mutations: edits=%v deletes=%v", fake.edits, fake.deletes)
	}

	output := out.String()
	for _, want := range []string{"== gunk.dev ==", "OK     A www", "== broken.dev ==", "CREATE A @ -> 5.6.7.8"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q; got:\n%s", want, output)
		}
	}
	if strings.Index(output, "== gunk.dev ==") > strings.Index(output, "== broken.dev ==") {
		t.Errorf("zones printed out of order; got:\n%s", output)
	}
}

func TestSyncZones_TwoZonesDryRun_MakesNoMutations(t *testing.T) {
	zones, err := parseZones([]byte(twoZoneInput))
	if err != nil {
		t.Fatalf("parseZones: %v", err)
	}
	fake := &fakePorkbun{byDomain: map[string][]porkbunRecord{
		// Stale TTL -> UPDATE; unmatched record -> DELETE under --prune.
		"gunk.dev": {
			{ID: "1", Name: "www.gunk.dev", Type: "A", Content: "1.2.3.4", TTL: "60"},
			{ID: "2", Name: "stale.gunk.dev", Type: "A", Content: "9.9.9.9", TTL: "300"},
		},
		"broken.dev": {},
	}}
	var out bytes.Buffer

	if err := syncZones(fake, zones, true /*prune*/, true /*dryRun*/, &out); err != nil {
		t.Fatalf("syncZones: %v", err)
	}

	if got := fake.mutationCount(); got != 0 {
		t.Fatalf("dry-run performed %d mutations (creates=%v, edits=%v, deletes=%v); want 0",
			got, fake.creates, fake.edits, fake.deletes)
	}
	output := out.String()
	for _, want := range []string{"UPDATE A www", "DELETE A stale", "CREATE A @ -> 5.6.7.8"} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q; got:\n%s", want, output)
		}
	}
}

// A failing zone stops the run so later zones are left untouched.
func TestSyncZones_StopsAtFirstError(t *testing.T) {
	zones, err := parseZones([]byte(twoZoneInput))
	if err != nil {
		t.Fatalf("parseZones: %v", err)
	}
	fake := &failingRetrieve{fail: "gunk.dev"}

	if err := syncZones(fake, zones, false, false, io.Discard); err == nil {
		t.Fatal("syncZones succeeded; want error")
	}
	if want := []string{"gunk.dev"}; !slices.Equal(fake.retrieves, want) {
		t.Errorf("retrieves = %v; want %v (later zones must not be touched)", fake.retrieves, want)
	}
}

type failingRetrieve struct {
	fakePorkbun
	fail string
}

func (f *failingRetrieve) retrieve(domain string) ([]porkbunRecord, error) {
	f.retrieves = append(f.retrieves, domain)
	if domain == f.fail {
		return nil, errors.New("boom")
	}
	return nil, nil
}

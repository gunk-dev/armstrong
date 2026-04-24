package main

import (
	"bytes"
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
// assert that --dry-run makes no mutations.
type fakePorkbun struct {
	existing []porkbunRecord
	creates  []createRequest
	edits    []editRequest
	deletes  []string
}

func (f *fakePorkbun) retrieve(domain string) ([]porkbunRecord, error) {
	return f.existing, nil
}

func (f *fakePorkbun) create(domain string, req createRequest) error {
	f.creates = append(f.creates, req)
	return nil
}

func (f *fakePorkbun) editByNameType(domain, recordType, subdomain string, req editRequest) error {
	f.edits = append(f.edits, req)
	return nil
}

func (f *fakePorkbun) deleteByID(domain, id string) error {
	f.deletes = append(f.deletes, id)
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

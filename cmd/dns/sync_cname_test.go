package main

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// zoneFake is a stateful porkbunMutator for one zone. Like Porkbun, it refuses
// a create whose name holds a record the new one can't coexist with under the
// CNAME rule, answering RECORD_CONFLICT. After every write it calls check, so
// a test can assert an invariant that must hold throughout the run.
type zoneFake struct {
	domain  string
	records []porkbunRecord
	nextID  int
	writes  []string
	check   func(records []porkbunRecord)
}

func newZoneFake(domain string, records ...porkbunRecord) *zoneFake {
	return &zoneFake{domain: domain, records: records, nextID: 1000}
}

func (f *zoneFake) fqdn(name string) string {
	if name == "" {
		return f.domain
	}
	return name + "." + f.domain
}

func (f *zoneFake) retrieve(domain string) ([]porkbunRecord, error) {
	return slices.Clone(f.records), nil
}

func (f *zoneFake) create(domain string, req createRequest) error {
	f.writes = append(f.writes, fmt.Sprintf("create %s %s %s", req.Type, displayName(req.Name), req.Content))
	for _, r := range f.records {
		if cnameConflict(req.Type, f.fqdn(req.Name), r.Type, r.Name) {
			return fmt.Errorf(`create failed: {"status":"ERROR","code":"RECORD_CONFLICT","conflictingRecords":[{"id":%q,"name":%q,"type":%q}]}`, r.ID, r.Name, r.Type)
		}
	}
	f.nextID++
	f.records = append(f.records, porkbunRecord{
		ID: strconv.Itoa(f.nextID), Name: f.fqdn(req.Name), Type: req.Type,
		Content: req.Content, TTL: req.TTL, Priority: req.Priority,
	})
	f.runCheck()
	return nil
}

func (f *zoneFake) editByNameType(domain, recordType, subdomain string, req editRequest) error {
	f.writes = append(f.writes, fmt.Sprintf("edit %s %s", recordType, displayName(subdomain)))
	for i, r := range f.records {
		if r.Type == recordType && r.Name == f.fqdn(subdomain) {
			f.records[i].Content, f.records[i].TTL, f.records[i].Priority = req.Content, req.TTL, req.Priority
		}
	}
	f.runCheck()
	return nil
}

func (f *zoneFake) deleteByID(domain, id string) error {
	f.writes = append(f.writes, "delete id="+id)
	f.records = slices.DeleteFunc(f.records, func(r porkbunRecord) bool { return r.ID == id })
	f.runCheck()
	return nil
}

func (f *zoneFake) runCheck() {
	if f.check != nil {
		f.check(f.records)
	}
}

// state renders the zone's records as sorted "TYPE name content" lines.
func (f *zoneFake) state() []string {
	var lines []string
	for _, r := range f.records {
		lines = append(lines, fmt.Sprintf("%s %s %s", r.Type, r.Name, r.Content))
	}
	slices.Sort(lines)
	return lines
}

func TestSyncRecords_AToCNAME(t *testing.T) {
	fake := newZoneFake("example.com",
		porkbunRecord{ID: "1", Name: "www.example.com", Type: "A", Content: "1.2.3.4", TTL: "600"},
	)
	input := dnsInput{Domain: "example.com", Records: []dnsRecord{
		{Type: "CNAME", Name: "www", Content: "app.fly.dev", TTL: 600},
	}}

	if err := syncRecords(fake, input, true, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("syncRecords: %v", err)
	}
	if want := []string{"delete id=1", "create CNAME www app.fly.dev"}; !slices.Equal(fake.writes, want) {
		t.Errorf("writes = %v; want %v", fake.writes, want)
	}
	if want := []string{"CNAME www.example.com app.fly.dev"}; !slices.Equal(fake.state(), want) {
		t.Errorf("final records = %v; want %v", fake.state(), want)
	}
}

func TestSyncRecords_CNAMEToA(t *testing.T) {
	fake := newZoneFake("example.com",
		porkbunRecord{ID: "1", Name: "www.example.com", Type: "CNAME", Content: "app.fly.dev", TTL: "600"},
	)
	input := dnsInput{Domain: "example.com", Records: []dnsRecord{
		{Type: "A", Name: "www", Content: "1.2.3.4", TTL: 600},
		{Type: "AAAA", Name: "www", Content: "::1", TTL: 600},
	}}

	if err := syncRecords(fake, input, true, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("syncRecords: %v", err)
	}
	if want := []string{"delete id=1", "create A www 1.2.3.4", "create AAAA www ::1"}; !slices.Equal(fake.writes, want) {
		t.Errorf("writes = %v; want %v", fake.writes, want)
	}
	if want := []string{"A www.example.com 1.2.3.4", "AAAA www.example.com ::1"}; !slices.Equal(fake.state(), want) {
		t.Errorf("final records = %v; want %v", fake.state(), want)
	}
}

// infraZone is broken.dev before and after gunk-dev/infra#94: the apex A
// moves to a new address and gains an AAAA, and www goes from an A to a CNAME.
func infraZone() (*zoneFake, dnsInput) {
	fake := newZoneFake("broken.dev",
		porkbunRecord{ID: "1", Name: "broken.dev", Type: "A", Content: "34.117.196.131", TTL: "600"},
		porkbunRecord{ID: "582930380", Name: "www.broken.dev", Type: "A", Content: "34.117.196.131", TTL: "600"},
		porkbunRecord{ID: "3", Name: "broken.dev", Type: "NS", Content: "curitiba.ns.porkbun.com", TTL: "86400"},
	)
	input := dnsInput{Domain: "broken.dev", Records: []dnsRecord{
		{Type: "A", Name: "", Content: "66.241.125.235", TTL: 600},
		{Type: "AAAA", Name: "", Content: "2a09:8280:1::f8:c96e:0", TTL: 600},
		{Type: "CNAME", Name: "www", Content: "valley-web-prod.fly.dev", TTL: 600},
	}}
	return fake, input
}

func TestSyncRecords_InfraApexChangeAndWWWToCNAME(t *testing.T) {
	fake, input := infraZone()
	fake.check = func(records []porkbunRecord) {
		apexA := 0
		for _, r := range records {
			if r.Type == "A" && r.Name == "broken.dev" {
				apexA++
			}
		}
		if apexA == 0 {
			t.Errorf("apex has no A record after %q", fake.writes[len(fake.writes)-1])
		}
	}

	if err := syncRecords(fake, input, true, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("syncRecords: %v", err)
	}
	want := []string{
		"A broken.dev 66.241.125.235",
		"AAAA broken.dev 2a09:8280:1::f8:c96e:0",
		"CNAME www.broken.dev valley-web-prod.fly.dev",
		"NS broken.dev curitiba.ns.porkbun.com",
	}
	if !slices.Equal(fake.state(), want) {
		t.Errorf("final records = %v; want %v", fake.state(), want)
	}
}

func TestSyncRecords_InfraDryRunPrintsWriteOrder(t *testing.T) {
	fake, input := infraZone()
	var out bytes.Buffer

	if err := syncRecords(fake, input, true, true, &out); err != nil {
		t.Fatalf("syncRecords: %v", err)
	}
	if len(fake.writes) != 0 {
		t.Fatalf("dry run wrote %v", fake.writes)
	}
	want := []string{
		"DRY RUN — no changes will be made",
		"DELETE A www -> 34.117.196.131 (id=582930380)",
		"CREATE A @ -> 66.241.125.235 (ttl=600)",
		"CREATE AAAA @ -> 2a09:8280:1::f8:c96e:0 (ttl=600)",
		"CREATE CNAME www -> valley-web-prod.fly.dev (ttl=600)",
		"DELETE A @ -> 34.117.196.131 (id=1)",
	}
	if got := strings.Split(strings.TrimSpace(out.String()), "\n"); !slices.Equal(got, want) {
		t.Errorf("dry-run output:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The real run writes in the order the dry run printed.
	if err := syncRecords(fake, input, true, false, &bytes.Buffer{}); err != nil {
		t.Fatalf("syncRecords: %v", err)
	}
	wantWrites := []string{
		"delete id=582930380",
		"create A @ 66.241.125.235",
		"create AAAA @ 2a09:8280:1::f8:c96e:0",
		"create CNAME www valley-web-prod.fly.dev",
		"delete id=1",
	}
	if !slices.Equal(fake.writes, wantWrites) {
		t.Errorf("writes = %v; want %v", fake.writes, wantWrites)
	}
}

func TestSyncRecords_NoPruneConflictRefusedBeforeAnyWrite(t *testing.T) {
	fake, input := infraZone()
	var out bytes.Buffer

	err := syncRecords(fake, input, false, false, &out)
	if err == nil {
		t.Fatal("syncRecords succeeded; want a CNAME conflict error")
	}
	if len(fake.writes) != 0 {
		t.Errorf("writes = %v; want none", fake.writes)
	}
	for _, want := range []string{"CNAME www -> valley-web-prod.fly.dev", "A www -> 34.117.196.131 (id=582930380)", "--prune"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestSyncRecords_CNAMEConflictWithProtectedRecordRefused(t *testing.T) {
	fake := newZoneFake("example.com",
		porkbunRecord{ID: "1", Name: "preview-7.example.com", Type: "TXT", Content: "x", TTL: "600"},
	)
	input := dnsInput{Domain: "example.com", Records: []dnsRecord{
		{Type: "A", Name: "a", Content: "1.2.3.4", TTL: 600},
		{Type: "CNAME", Name: "preview-7", Content: "app.fly.dev", TTL: 600},
	}}

	err := syncRecords(fake, input, true, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "TXT preview-7") {
		t.Fatalf("err = %v; want a conflict naming TXT preview-7", err)
	}
	if strings.Contains(err.Error(), "--prune") {
		t.Errorf("error %q suggests --prune, which never deletes preview-* records", err)
	}
	if len(fake.writes) != 0 {
		t.Errorf("writes = %v; want none", fake.writes)
	}
}

func TestSyncRecords_DesiredCNAMEConflictRefusedBeforeAnyWrite(t *testing.T) {
	fake := newZoneFake("example.com")
	input := dnsInput{Domain: "example.com", Records: []dnsRecord{
		{Type: "A", Name: "api", Content: "1.2.3.4", TTL: 600},
		{Type: "CNAME", Name: "www", Content: "app.fly.dev", TTL: 600},
		{Type: "TXT", Name: "www", Content: "v=spf1", TTL: 600},
	}}

	err := syncRecords(fake, input, true, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "CNAME www -> app.fly.dev and TXT www -> v=spf1") {
		t.Fatalf("err = %v; want a desired-state conflict naming both records", err)
	}
	if len(fake.writes) != 0 {
		t.Errorf("writes = %v; want none", fake.writes)
	}
}

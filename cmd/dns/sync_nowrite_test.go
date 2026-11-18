package main

import (
	"errors"
	"io"
	"testing"
)

// failingCreates fails every create, after passing it to the fake.
type failingCreates struct{ *fakePorkbun }

func (f failingCreates) create(domain string, req createRequest) error {
	f.fakePorkbun.create(domain, req)
	return errors.New("create failed")
}

// TestSyncCounted_NoWrite covers which failures leave the records unchanged:
// only one before any write was attempted. An attempted write counts even if
// it failed, since its request may have landed.
func TestSyncCounted_NoWrite(t *testing.T) {
	// b.dev, synced first in the last case, prunes its stale record.
	stale := map[string][]porkbunRecord{"b.dev": {{ID: "9", Name: "old.b.dev", Type: "A", Content: "9.9.9.9", TTL: "600"}}}
	conflict := dnsInput{Domain: "gunk.dev", Records: []dnsRecord{
		{Type: "CNAME", Name: "www", Content: "a.example", TTL: 600},
		{Type: "A", Name: "www", Content: "1.2.3.4", TTL: 600},
	}}
	create := dnsInput{Domain: "gunk.dev", Records: []dnsRecord{{Type: "A", Name: "new", Content: "1.2.3.4", TTL: 600}}}
	for _, tc := range []struct {
		name    string
		client  porkbunMutator
		zones   []dnsInput
		noWrite bool
	}{
		{"refused before any write", &fakePorkbun{}, []dnsInput{conflict}, true},
		{"first write fails", failingCreates{&fakePorkbun{}}, []dnsInput{create}, false},
		{"fails after a write", failingCreates{&fakePorkbun{byDomain: stale}}, []dnsInput{{Domain: "b.dev"}, create}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := syncCounted(tc.client, tc.zones, true, false, io.Discard)
			if err == nil {
				t.Fatal("sync succeeded, want a failure")
			}
			if got := errors.As(err, new(errNoWrite)); got != tc.noWrite {
				t.Errorf("no write = %v (%v), want %v", got, err, tc.noWrite)
			}
		})
	}
}

package main

import "testing"

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

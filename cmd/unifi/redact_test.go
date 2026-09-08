package main

import (
	"strings"
	"testing"
)

// Redaction is the one piece of string handling here subtle enough to deserve
// a unit test: the same secret reaches the output at two different levels of
// JSON quoting, and getting the escaping wrong fails open.
func TestRedactJSONString(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{{
		name: "plain json",
		in:   `{"type":"WPA2_PERSONAL","passphrase":"hunter2","enabled":true}`,
		want: `{"type":"WPA2_PERSONAL","passphrase":"[REDACTED]","enabled":true}`,
	}, {
		name: "escaped inside an error message",
		in:   `{"message":"rejected: {\"passphrase\":\"hunter2\",\"type\":\"WPA2\"}"}`,
		want: `{"message":"rejected: {\"passphrase\":\"[REDACTED]\",\"type\":\"WPA2\"}"}`,
	}, {
		name: "space after the colon",
		in:   `{"passphrase": "hunter2"}`,
		want: `{"passphrase": "[REDACTED]"}`,
	}, {
		name: "several occurrences",
		in:   `[{"passphrase":"one"},{"passphrase":"two"}]`,
		want: `[{"passphrase":"[REDACTED]"},{"passphrase":"[REDACTED]"}]`,
	}, {
		name: "value containing an escaped quote",
		in:   `{"passphrase":"hun\"ter","name":"visible"}`,
		want: `{"passphrase":"[REDACTED]","name":"visible"}`,
	}, {
		name: "unterminated value is redacted to the end",
		in:   `{"passphrase":"hunter2`,
		want: `{"passphrase":"[REDACTED]`,
	}, {
		name: "nothing to redact",
		in:   `{"name":"guest","enabled":false}`,
		want: `{"name":"guest","enabled":false}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactJSONString(tc.in, "passphrase"); got != tc.want {
				t.Errorf("redactJSONString()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// A registered secret must be scrubbed wherever it appears, whatever the
// surrounding quoting — this is the backstop for payloads the API echoes.
func TestRegisteredSecretsAreRedactedAnywhere(t *testing.T) {
	const secret = "correct-horse-battery-staple"
	registerSecret(secret)

	for _, in := range []string{
		secret,
		`{"securityConfiguration":{"passphrase":"` + secret + `"}}`,
		`rejected payload: {\"securityConfiguration\":{\"passphrase\":\"` + secret + `\"}}`,
		"plain prose mentioning " + secret + " in passing",
	} {
		if got := redact(in); strings.Contains(got, secret) {
			t.Errorf("secret survived redaction:\n in: %s\nout: %s", in, got)
		}
	}

	// Short values are not registered: blanking them would mangle ordinary text.
	registerSecret("ok")
	if got := redact("this is ok"); got != "this is ok" {
		t.Errorf("a too-short value was treated as a secret: %q", got)
	}
}

func TestPassphraseEnvFor(t *testing.T) {
	for _, tc := range []struct{ ssid, security, want string }{
		{"main", "WPA2_PERSONAL", "UNIFI_WIFI_MAIN"},
		{"Guest Wi-Fi 5", "WPA3_PERSONAL", "UNIFI_WIFI_GUEST_WI_FI_5"},
		{"open-net", "OPEN", ""},
	} {
		if got := passphraseEnvFor(tc.ssid, tc.security); got != tc.want {
			t.Errorf("passphraseEnvFor(%q, %q) = %q, want %q", tc.ssid, tc.security, got, tc.want)
		}
	}
}

// Firewall ports are written as "443" or "8000-8100" and have to become the
// two different item shapes the API distinguishes.
func TestPortItems(t *testing.T) {
	items := portItems([]string{"443", "8000-8100"})
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0]["type"] != "PORT_NUMBER" || items[0]["port"] != 443 {
		t.Errorf("single port rendered as %v", items[0])
	}
	if items[1]["type"] != "PORT_NUMBER_RANGE" || items[1]["startPort"] != 8000 || items[1]["endPort"] != 8100 {
		t.Errorf("port range rendered as %v", items[1])
	}
}

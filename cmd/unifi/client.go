package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// client talks to the official UniFi Network Integration API served by the
// console at <base>/proxy/network/integration/v1.
type client struct {
	base   string
	apiKey string
	http   *http.Client
}

const apiPrefix = "/proxy/network/integration/v1"

// newClient builds a client from the environment:
//
//	UNIFI_URL           console base URL, e.g. https://192.168.1.1
//	UNIFI_API_KEY       Integration API key
//	UNIFI_CA_FILE       PEM bundle for the console's self-signed cert
//	UNIFI_INSECURE_TLS  set to 1 to skip certificate verification instead
func newClient() (*client, error) {
	base := strings.TrimSuffix(os.Getenv("UNIFI_URL"), "/")
	key := os.Getenv("UNIFI_API_KEY")
	if base == "" || key == "" {
		return nil, fmt.Errorf("UNIFI_URL and UNIFI_API_KEY must be set")
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("parse UNIFI_URL: %w", err)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile := os.Getenv("UNIFI_CA_FILE"); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read UNIFI_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("UNIFI_CA_FILE %s contains no PEM certificates", caFile)
		}
		tlsCfg.RootCAs = pool
	} else if os.Getenv("UNIFI_INSECURE_TLS") == "1" {
		tlsCfg.InsecureSkipVerify = true
	}

	return &client{
		base:   base,
		apiKey: key,
		http: &http.Client{
			Timeout:   60 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// do issues a request against the Integration API. body may be nil; out may be
// nil to discard the response.
func (c *client) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, c.base+apiPrefix+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Error strings from net/http can embed the URL but never headers,
		// so the API key cannot leak here.
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{
			Method: method,
			Path:   path,
			Status: resp.Status,
			Code:   errorCode(data),
			Body:   redact(string(data)),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: parse response: %w", method, path, err)
	}
	return nil
}

// apiError is a non-2xx response. The API reports application-level failures
// with a machine-readable `code`, which callers branch on instead of matching
// the human-readable message.
type apiError struct {
	Method string
	Path   string
	Status string
	Code   string
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: %s: %s", e.Method, e.Path, e.Status, e.Body)
}

// errorCode pulls the `code` out of an error body. Authentication failures use
// a different, nested envelope (`{"error":{"code":401,…}}`) whose code is a
// number, not a string; those simply yield "".
func errorCode(data []byte) string {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return ""
	}
	return body.Code
}

// hasErrorCode reports whether err is an API error carrying the given code.
func hasErrorCode(err error, code string) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Code == code
}

type page struct {
	Offset     int               `json:"offset"`
	Limit      int               `json:"limit"`
	Count      int               `json:"count"`
	TotalCount int               `json:"totalCount"`
	Data       []json.RawMessage `json:"data"`
}

const pageLimit = 200

// list walks the API's offset/limit paging and returns every element.
func (c *client) list(path string) ([]json.RawMessage, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var all []json.RawMessage
	for offset := 0; ; {
		var p page
		url := fmt.Sprintf("%s%slimit=%d&offset=%d", path, sep, pageLimit, offset)
		if err := c.do(http.MethodGet, url, nil, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Data...)
		offset += len(p.Data)
		if len(p.Data) == 0 || offset >= p.TotalCount {
			return all, nil
		}
	}
}

type siteRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// siteID resolves a site name (UNIFI_SITE, default "Default") to its id.
func (c *client) siteID(name string) (string, error) {
	raws, err := c.list("/sites")
	if err != nil {
		return "", fmt.Errorf("list sites: %w", err)
	}
	var names []string
	for _, raw := range raws {
		var s siteRef
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("parse site: %w", err)
		}
		if s.Name == name {
			return s.ID, nil
		}
		names = append(names, s.Name)
	}
	return "", fmt.Errorf("site %q not found (have: %s)", name, strings.Join(names, ", "))
}

func siteName() string {
	if s := os.Getenv("UNIFI_SITE"); s != "" {
		return s
	}
	return "Default"
}

// secrets holds values that must never appear in output: every wifi passphrase
// the tool reads from the console or resolves from the environment. Redacting
// by value as well as by field name is what makes the API's own error
// responses safe to print — when a console rejects a payload it quotes it back
// escaped inside a JSON string, where a `"passphrase":"…"` scrubber finds
// nothing to match.
var secrets struct {
	mu   sync.RWMutex
	vals []string
}

// registerSecret marks a value for redaction from all later output. Very short
// values are ignored: they are not real passphrases and would blank out
// unrelated text.
func registerSecret(v string) {
	if len(v) < 6 {
		return
	}
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	for _, existing := range secrets.vals {
		if existing == v {
			return
		}
	}
	secrets.vals = append(secrets.vals, v)
}

// redact strips anything that looks like a secret out of text that is about to
// be printed: known passphrase values, the passphrase fields the API returns,
// and the API key itself.
func redact(s string) string {
	secrets.mu.RLock()
	for _, v := range secrets.vals {
		s = strings.ReplaceAll(s, v, redacted)
	}
	secrets.mu.RUnlock()

	for _, field := range []string{"passphrase", "xPassphrase", "presharedKey"} {
		s = redactJSONString(s, field)
	}
	if key := os.Getenv("UNIFI_API_KEY"); key != "" {
		s = strings.ReplaceAll(s, key, redacted)
	}
	return s
}

const redacted = "[REDACTED]"

// redactJSONString replaces the value of every `"<field>": "..."` occurrence.
// The pair may appear at either of two levels of quoting: plainly in a JSON
// document, or backslash-escaped inside a JSON string when the API echoes a
// rejected payload back in an error message.
func redactJSONString(s, field string) string {
	for _, quote := range []string{`"`, `\"`} {
		s = redactQuoted(s, field, quote)
	}
	return s
}

func redactQuoted(s, field, quote string) string {
	needle := quote + field + quote + ":"
	var b strings.Builder
	for {
		i := strings.Index(s, needle)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(needle)])
		rest := s[i+len(needle):]

		j := 0
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
			j++
		}
		if !strings.HasPrefix(rest[j:], quote) {
			// Not a string value (a number, null, an object) — leave it be.
			b.WriteString(rest[:j])
			s = rest[j:]
			continue
		}
		b.WriteString(rest[:j+len(quote)])
		rest = rest[j+len(quote):]

		b.WriteString(redacted)
		end := endOfQuoted(rest, quote)
		if end < 0 {
			return b.String()
		}
		s = rest[end:]
	}
}

// endOfQuoted returns the offset of the quote closing a string value, skipping
// escaped characters, or -1 if the value is not terminated.
func endOfQuoted(s, quote string) int {
	for i := 0; i < len(s); {
		if s[i] == '\\' {
			// A backslash escapes what follows — unless what follows is the
			// escaped-level closing quote itself.
			if strings.HasPrefix(s[i:], quote) {
				return i
			}
			i += 2
			continue
		}
		if strings.HasPrefix(s[i:], quote) {
			return i
		}
		i++
	}
	return -1
}

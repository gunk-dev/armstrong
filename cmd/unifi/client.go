package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
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
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, redact(string(data)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: parse response: %w", method, path, err)
	}
	return nil
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

// redact strips anything that looks like a secret out of text that is about to
// be printed: the passphrase fields the API returns, and the API key itself.
func redact(s string) string {
	for _, field := range []string{"passphrase", "xPassphrase", "presharedKey"} {
		s = redactJSONString(s, field)
	}
	if key := os.Getenv("UNIFI_API_KEY"); key != "" {
		s = strings.ReplaceAll(s, key, "[REDACTED]")
	}
	return s
}

// redactJSONString replaces the value of every `"<field>":"..."` occurrence.
func redactJSONString(s, field string) string {
	needle := `"` + field + `":"`
	var b strings.Builder
	for {
		i := strings.Index(s, needle)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+len(needle)])
		rest := s[i+len(needle):]
		end := 0
		for end < len(rest) {
			if rest[end] == '\\' {
				end += 2
				continue
			}
			if rest[end] == '"' {
				break
			}
			end++
		}
		if end >= len(rest) {
			b.WriteString("[REDACTED]")
			return b.String()
		}
		b.WriteString("[REDACTED]")
		s = rest[end:]
	}
}

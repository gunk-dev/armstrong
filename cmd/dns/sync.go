package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type dnsInput struct {
	Domain  string      `json:"domain"`
	Records []dnsRecord `json:"records"`
}

type dnsRecord struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority"`
}

// porkbunMutator is the subset of the Porkbun client used by sync. It exists
// so tests can inject a fake and verify that --dry-run makes no mutations.
type porkbunMutator interface {
	retrieve(domain string) ([]porkbunRecord, error)
	create(domain string, req createRequest) error
	editByNameType(domain, recordType, subdomain string, req editRequest) error
	deleteByID(domain, id string) error
}

func newSyncCmd() *cobra.Command {
	var prune bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync DNS records from CUE definition to Porkbun",
		Long: "Reads JSON from stdin (pipe from: cue export ./dns --out json) and converges Porkbun records to match.\n" +
			"Accepts either a single zone object or a JSON array of zone objects, which are synced in order.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(prune, dryRun, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVar(&prune, "prune", false, "Delete records not in CUE definition (skips NS, SOA, and preview-* records)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the changes that would be made without calling the Porkbun API")
	return cmd
}

func runSync(prune, dryRun bool, stdin io.Reader, out io.Writer) error {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	zones, err := parseZones(data)
	if err != nil {
		return err
	}

	client, err := newPorkbunClient()
	if err != nil {
		return err
	}

	return syncZones(client, zones, prune, dryRun, out)
}

// parseZones accepts either a single zone object (one domain) or a JSON array
// of them, so a caller repo can manage several domains from one CUE tree. The
// zones it returns are guaranteed to have non-empty, mutually distinct domains.
func parseZones(data []byte) ([]dnsInput, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("parse input: empty input")
	}

	switch trimmed[0] {
	case '[':
		var zones []dnsInput
		if err := json.Unmarshal(trimmed, &zones); err != nil {
			return nil, fmt.Errorf("parse input: %w", err)
		}
		if len(zones) == 0 {
			return nil, fmt.Errorf("no zones in input")
		}
		if err := validateZones(zones); err != nil {
			return nil, err
		}
		return zones, nil
	case '{':
		var zone dnsInput
		if err := json.Unmarshal(trimmed, &zone); err != nil {
			return nil, fmt.Errorf("parse input: %w", err)
		}
		zones := []dnsInput{zone}
		if err := validateZones(zones); err != nil {
			return nil, err
		}
		return zones, nil
	default:
		return nil, fmt.Errorf("parse input: expected a JSON object or array of objects, got %q", trimmed[0])
	}
}

// validateZones rejects input that syncZones cannot sync coherently. Two
// entries for the same domain (a copied dns/<zone>/ directory whose domain was
// not updated) both pass CUE validation, but with --prune each pass would
// delete the records declared only by the other, so the whole run is refused
// before any API call. An empty domain is rejected too: it would otherwise be
// sent to the API as a malformed URL.
func validateZones(zones []dnsInput) error {
	seen := map[string]bool{}
	for _, zone := range zones {
		domain := normalizeDomain(zone.Domain)
		if domain == "" {
			return fmt.Errorf("zone with empty domain in input")
		}
		if seen[domain] {
			return fmt.Errorf("duplicate zone %q in input", domain)
		}
		seen[domain] = true
	}
	return nil
}

// normalizeDomain canonicalizes a domain for comparison: DNS names are
// case-insensitive and "gunk.dev." names the same zone as "gunk.dev".
func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// syncZones syncs each zone in order, stopping at the first failure.
func syncZones(client porkbunMutator, zones []dnsInput, prune, dryRun bool, out io.Writer) error {
	for _, zone := range zones {
		fmt.Fprintf(out, "== %s ==\n", zone.Domain)
		if err := syncRecords(client, zone, prune, dryRun, out); err != nil {
			return err
		}
	}
	return nil
}

// syncOp is one line of a zone's sync plan: an OK, or a write and the text
// that announces it. write is nil for OK.
type syncOp struct {
	line  string
	write func() error
}

// syncRecords converges one zone. It plans every create, update and delete
// before making any write, refuses a plan that would break the CNAME rule
// (RFC 1034: a CNAME cannot coexist with any other record at its name), and
// then runs the writes in an order where no create meets a conflicting record:
//
//  1. deletes that clear a name for a create that conflicts with them — the
//     pruned records at a name that gains a CNAME, and a pruned CNAME at a
//     name that gains any other record;
//  2. creates and updates, in input order;
//  3. every other delete, so an ordinary content change keeps its old record
//     until the new one exists.
//
// --dry-run prints the same lines in the same order without writing.
func syncRecords(client porkbunMutator, input dnsInput, prune, dryRun bool, out io.Writer) error {
	if err := checkDesiredCNAMEs(input.Records); err != nil {
		return fmt.Errorf("%s: %w", input.Domain, err)
	}

	existing, err := client.retrieve(input.Domain)
	if err != nil {
		return err
	}

	type recordKey struct {
		Type    string
		Name    string
		Content string
	}

	existingByKey := map[recordKey]porkbunRecord{}
	for _, r := range existing {
		// Porkbun returns FQDN in name, strip the domain suffix
		name := stripDomain(r.Name, input.Domain)
		key := recordKey{Type: r.Type, Name: name, Content: normalizeContent(r.Type, r.Content)}
		existingByKey[key] = r
	}

	// Track which existing records are accounted for
	matched := map[string]bool{} // by record ID

	var upserts []syncOp
	var creates []dnsRecord
	for _, want := range input.Records {
		key := recordKey{Type: want.Type, Name: want.Name, Content: normalizeContent(want.Type, want.Content)}
		if got, ok := existingByKey[key]; ok {
			matched[got.ID] = true
			// Check if TTL or priority needs updating
			gotTTL, _ := strconv.Atoi(got.TTL)
			gotPrio, _ := strconv.Atoi(got.Priority)
			wantPrio := 0
			if want.Priority != nil {
				wantPrio = *want.Priority
			}
			if gotTTL != want.TTL || (want.Priority != nil && gotPrio != wantPrio) {
				req := editRequest{
					Content: want.Content,
					TTL:     strconv.Itoa(want.TTL),
				}
				if want.Priority != nil {
					req.Priority = strconv.Itoa(*want.Priority)
				}
				upserts = append(upserts, syncOp{
					line: fmt.Sprintf("UPDATE %s %s -> %s (ttl=%d%s)", want.Type, displayName(want.Name), want.Content, want.TTL, prioSuffix(want.Priority)),
					write: func() error {
						if err := client.editByNameType(input.Domain, want.Type, want.Name, req); err != nil {
							return fmt.Errorf("update %s %s: %w", want.Type, displayName(want.Name), err)
						}
						return nil
					},
				})
			} else {
				upserts = append(upserts, syncOp{line: fmt.Sprintf("OK     %s %s -> %s", want.Type, displayName(want.Name), want.Content)})
			}
		} else {
			creates = append(creates, want)
			req := createRequest{
				Type:    want.Type,
				Name:    want.Name,
				Content: want.Content,
				TTL:     strconv.Itoa(want.TTL),
			}
			if want.Priority != nil {
				req.Priority = strconv.Itoa(*want.Priority)
			}
			upserts = append(upserts, syncOp{
				line: fmt.Sprintf("CREATE %s %s -> %s (ttl=%d%s)", want.Type, displayName(want.Name), want.Content, want.TTL, prioSuffix(want.Priority)),
				write: func() error {
					if err := client.create(input.Domain, req); err != nil {
						return fmt.Errorf("create %s %s: %w", want.Type, displayName(want.Name), err)
					}
					return nil
				},
			})
		}
	}

	// prunable reports whether --prune deletes r: every unmatched record
	// except NS, SOA and preview-*.
	prunable := func(r porkbunRecord) bool {
		if matched[r.ID] || r.Type == "NS" || r.Type == "SOA" {
			return false
		}
		return !strings.HasPrefix(stripDomain(r.Name, input.Domain), "preview-")
	}
	pruned := map[string]bool{} // by record ID
	if prune {
		for _, r := range existing {
			if prunable(r) {
				pruned[r.ID] = true
			}
		}
	}

	// Every create that meets a conflicting existing record needs that record
	// deleted first. If the plan doesn't delete it, refuse before any write.
	early := map[string]bool{} // by record ID
	for _, want := range creates {
		for _, r := range existing {
			name := stripDomain(r.Name, input.Domain)
			if !cnameConflict(want.Type, want.Name, r.Type, name) {
				continue
			}
			if !pruned[r.ID] {
				hint := ""
				if !prune && prunable(r) {
					hint = " (run with --prune to replace it)"
				}
				return fmt.Errorf("%s: CNAME conflict: desired %s %s -> %s conflicts with existing %s %s -> %s (id=%s), which this sync would not delete%s",
					input.Domain, want.Type, displayName(want.Name), want.Content, r.Type, displayName(name), r.Content, r.ID, hint)
			}
			early[r.ID] = true
		}
	}

	var first, last []syncOp
	for _, r := range existing {
		if !pruned[r.ID] {
			continue
		}
		op := syncOp{
			line: fmt.Sprintf("DELETE %s %s -> %s (id=%s)", r.Type, displayName(stripDomain(r.Name, input.Domain)), r.Content, r.ID),
			write: func() error {
				if err := client.deleteByID(input.Domain, r.ID); err != nil {
					return fmt.Errorf("delete %s (id=%s): %w", r.Type, r.ID, err)
				}
				return nil
			},
		}
		if early[r.ID] {
			first = append(first, op)
		} else {
			last = append(last, op)
		}
	}

	if dryRun {
		fmt.Fprintln(out, "DRY RUN — no changes will be made")
	}
	for _, phase := range [][]syncOp{first, upserts, last} {
		for _, op := range phase {
			fmt.Fprintln(out, op.line)
			if dryRun || op.write == nil {
				continue
			}
			if err := op.write(); err != nil {
				return err
			}
		}
	}
	return nil
}

// cnameConflict reports whether records of these types and names can't
// coexist: they share a name and at least one of them is a CNAME.
func cnameConflict(typeA, nameA, typeB, nameB string) bool {
	if !strings.EqualFold(nameA, nameB) {
		return false
	}
	return typeA == "CNAME" || typeB == "CNAME"
}

// checkDesiredCNAMEs refuses a desired zone that itself breaks the CNAME rule:
// a CNAME alongside any other record at the same name. The CUE schema checks
// each record on its own, so this is the first place the rule can be checked.
func checkDesiredCNAMEs(records []dnsRecord) error {
	for i, a := range records {
		for _, b := range records[i+1:] {
			if cnameConflict(a.Type, a.Name, b.Type, b.Name) {
				return fmt.Errorf("CNAME conflict in desired records: %s %s -> %s and %s %s -> %s share a name",
					a.Type, displayName(a.Name), a.Content, b.Type, displayName(b.Name), b.Content)
			}
		}
	}
	return nil
}

func stripDomain(fqdn, domain string) string {
	suffix := "." + domain
	if fqdn == domain {
		return ""
	}
	return strings.TrimSuffix(fqdn, suffix)
}

func displayName(name string) string {
	if name == "" {
		return "@"
	}
	return name
}

func prioSuffix(priority *int) string {
	if priority == nil {
		return ""
	}
	return fmt.Sprintf(", prio=%d", *priority)
}

// normalizeContent canonicalizes the content string for comparison.
// For AAAA records, it parses and re-serializes the IPv6 address so that
// different textual representations of the same address match.
func normalizeContent(recordType, content string) string {
	if recordType == "AAAA" {
		if ip := net.ParseIP(content); ip != nil {
			return ip.String()
		}
	}
	return content
}

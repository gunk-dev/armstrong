package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
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
		Long:  "Reads JSON from stdin (pipe from: cue export ./dns --out json) and converges Porkbun records to match.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(prune, dryRun)
		},
	}

	cmd.Flags().BoolVar(&prune, "prune", false, "Delete records not in CUE definition (skips NS, SOA, and preview-* records)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the changes that would be made without calling the Porkbun API")
	return cmd
}

func runSync(prune, dryRun bool) error {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}

	var input dnsInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fmt.Errorf("parse input: %w", err)
	}

	client, err := newPorkbunClient()
	if err != nil {
		return err
	}

	return syncRecords(client, input, prune, dryRun, os.Stdout)
}

func syncRecords(client porkbunMutator, input dnsInput, prune, dryRun bool, out io.Writer) error {
	if dryRun {
		fmt.Fprintln(out, "DRY RUN — no changes will be made")
	}

	existing, err := client.retrieve(input.Domain)
	if err != nil {
		return err
	}

	// Build lookup of existing records: key -> []porkbunRecord
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

	// Create or update desired records
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
				fmt.Fprintf(out, "UPDATE %s %s -> %s (ttl=%d)\n", want.Type, displayName(want.Name), want.Content, want.TTL)
				req := editRequest{
					Content: want.Content,
					TTL:     strconv.Itoa(want.TTL),
				}
				if want.Priority != nil {
					req.Priority = strconv.Itoa(*want.Priority)
				}
				if !dryRun {
					if err := client.editByNameType(input.Domain, want.Type, want.Name, req); err != nil {
						return fmt.Errorf("update %s %s: %w", want.Type, want.Name, err)
					}
				}
			} else {
				fmt.Fprintf(out, "OK     %s %s -> %s\n", want.Type, displayName(want.Name), want.Content)
			}
		} else {
			fmt.Fprintf(out, "CREATE %s %s -> %s\n", want.Type, displayName(want.Name), want.Content)
			req := createRequest{
				Type:    want.Type,
				Name:    want.Name,
				Content: want.Content,
				TTL:     strconv.Itoa(want.TTL),
			}
			if want.Priority != nil {
				req.Priority = strconv.Itoa(*want.Priority)
			}
			if !dryRun {
				if err := client.create(input.Domain, req); err != nil {
					return fmt.Errorf("create %s %s: %w", want.Type, want.Name, err)
				}
			}
		}
	}

	// Prune records not in desired state
	if prune {
		for _, r := range existing {
			if matched[r.ID] {
				continue
			}
			// Never prune NS or SOA
			if r.Type == "NS" || r.Type == "SOA" {
				continue
			}
			// Never prune preview-* records
			name := stripDomain(r.Name, input.Domain)
			if strings.HasPrefix(name, "preview-") {
				continue
			}
			fmt.Fprintf(out, "DELETE %s %s -> %s (id=%s)\n", r.Type, displayName(name), r.Content, r.ID)
			if !dryRun {
				if err := client.deleteByID(input.Domain, r.ID); err != nil {
					return fmt.Errorf("delete %s (id=%s): %w", r.Type, r.ID, err)
				}
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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guards of #18: a sync that would delete something the instance file
// has not approved, or change too much at once, must refuse before its first
// write, and a sync that does write must leave a snapshot `restore` can undo
// it with.

// seedExtraRecord adds a second USER_DEFINED DNS record next to seedSite's.
func seedExtraRecord(f *fakeConsole) {
	f.seed(collDNS, originUser, map[string]any{
		"type": "A_RECORD", "enabled": true,
		"domain": "old.example.internal", "ipv4Address": "192.0.2.20", "ttlSeconds": 0,
	})
}

// pruneBothRecords declares dnsPolicies with one new record, so both seeded
// records are prune candidates; deletions is spliced in.
const pruneBothRecords = `{
  "dnsPolicies": [{"type":"A_RECORD","enabled":true,"domain":"new.example.internal","ipv4Address":"192.0.2.30","ttlSeconds":0}],
  "deletions": [%s]
}`

func snapshots(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "snapshot-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestDiffMarksListedAndUnlistedDeletions(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	seedExtraRecord(f)

	desired := fmt.Sprintf(pruneBothRecords, `"dns policy A_RECORD nas.example.internal"`)
	stdout, stderr, code := run(t, f, desired, nil, "diff", "--prune")
	if code != exitChangesPending {
		t.Fatalf("diff exited %d, want %d\n%s\n%s", code, exitChangesPending, stdout, stderr)
	}
	for _, want := range []string{
		"DELETE dns policy     A_RECORD nas.example.internal (listed in deletions)",
		"DELETE dns policy     A_RECORD old.example.internal (NOT listed in deletions)",
		"sync would refuse this plan",
		`"dns policy A_RECORD old.example.internal"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("diff output is missing %q:\n%s", want, stdout)
		}
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Errorf("diff mutated the console: %+v", muts)
	}
}

// An unlisted deletion refuses the whole run: not only is that object kept,
// the listed deletion and the create in the same plan are not made either,
// and no snapshot is taken because nothing is about to be written.
func TestUnlistedDeletionRefusesTheWholeSync(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	seedExtraRecord(f)
	dir := t.TempDir()

	desired := fmt.Sprintf(pruneBothRecords, `"dns policy A_RECORD nas.example.internal"`)
	stdout, stderr, code := run(t, f, desired, nil, "sync", "--prune", "--snapshot-dir", dir)
	if code != 1 {
		t.Fatalf("sync exited %d, want 1\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, `"dns policy A_RECORD old.example.internal"`) {
		t.Errorf("refusal does not name the key to add:\n%s", stderr)
	}
	if !strings.Contains(stdout, "NOT listed in deletions") {
		t.Errorf("refusal did not print the plan:\n%s", stdout)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Errorf("a refused sync wrote to the console: %+v", muts)
	}
	if s := snapshots(t, dir); len(s) != 0 {
		t.Errorf("a refused sync wrote a snapshot: %v", s)
	}

	// --force is the human override.
	mustRun(t, f, desired, nil, "sync", "--prune", "--force")
	if got := f.coll[collDNS].list(); len(got) != 1 {
		t.Errorf("dns records after --force = %v, want only the new one", got)
	}
}

func TestListedDeletionsAreAppliedAndStaleEntriesWarn(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	seedExtraRecord(f)

	desired := fmt.Sprintf(pruneBothRecords, `"dns policy A_RECORD nas.example.internal",
	  "dns policy A_RECORD old.example.internal", "wifi long-gone"`)
	stdout := mustRun(t, f, desired, nil, "sync", "--prune")

	var domains []string
	for _, rec := range f.coll[collDNS].list() {
		domains = append(domains, rec["domain"].(string))
	}
	if !equalStrings(domains, []string{"new.example.internal"}) {
		t.Errorf("dns records after sync = %v, want only the new one\n%s", domains, stdout)
	}
	if !strings.Contains(stdout, `WARN   deletions      "wifi long-gone" matches nothing`) {
		t.Errorf("stale deletions entry was not warned about:\n%s", stdout)
	}
}

func TestMaxChangesGuard(t *testing.T) {
	const records = 3
	desiredIP := func(ip string) string {
		var entries []string
		for i := range records {
			entries = append(entries, fmt.Sprintf(`{"type":"A_RECORD","enabled":true,"domain":"host%d.example.internal","ipv4Address":%q,"ttlSeconds":0}`, i, ip))
		}
		// One create alongside, which must not count towards the limit.
		entries = append(entries, `{"type":"A_RECORD","enabled":true,"domain":"extra.example.internal","ipv4Address":"192.0.2.99","ttlSeconds":0}`)
		return `{"dnsPolicies":[` + strings.Join(entries, ",") + `]}`
	}
	f := newFakeConsole(t)
	for i := range records {
		f.seed(collDNS, originUser, map[string]any{
			"type": "A_RECORD", "enabled": true,
			"domain": fmt.Sprintf("host%d.example.internal", i), "ipv4Address": "192.0.2.1", "ttlSeconds": 0,
		})
	}

	// N+1 updates against a limit of N.
	stdout, stderr, code := run(t, f, desiredIP("192.0.2.2"), nil, "sync", "--max-changes", fmt.Sprint(records-1))
	if code != 1 || !strings.Contains(stderr, "--max-changes") {
		t.Fatalf("guard did not trip (exit %d)\n%s\n%s", code, stdout, stderr)
	}
	if strings.Count(stdout, "UPDATE dns policy") != records || !strings.Contains(stdout, "CREATE dns policy") {
		t.Errorf("refusal did not print the full plan:\n%s", stdout)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Fatalf("a refused sync wrote to the console: %+v", muts)
	}

	// Exactly N is allowed; the create is not counted.
	mustRun(t, f, desiredIP("192.0.2.2"), nil, "sync", "--max-changes", fmt.Sprint(records))

	// And --force overrides the limit.
	mustRun(t, f, desiredIP("192.0.2.3"), nil, "sync", "--max-changes", "0", "--force")
	for _, rec := range f.coll[collDNS].list() {
		if strings.HasPrefix(rec["domain"].(string), "host") && rec["ipv4Address"] != "192.0.2.3" {
			t.Errorf("--force did not apply the plan: %v", rec)
		}
	}
}

// A sync that writes leaves a snapshot taken before its first write, and
// `restore` of that snapshot puts the site back as export saw it.
func TestSnapshotBeforeWriteAndRestoreRoundTrip(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	dir := t.TempDir()
	env := []string{"UNIFI_WIFI_MAIN=super-secret-passphrase"}
	before := mustRun(t, f, "", nil, "export")

	snapshotsAtFirstWrite := -1
	f.onMutation = func(mutation) {
		if snapshotsAtFirstWrite < 0 {
			snapshotsAtFirstWrite = len(snapshots(t, dir))
		}
	}

	// Update IoT, replace the DNS record (a listed delete plus a create), and
	// declare the SSID so the snapshot takes its passphraseEnv from here.
	desired := `{
	  "networks": [{"name":"IoT","management":"GATEWAY","enabled":true,"vlanId":30,
	    "isolationEnabled":true,"internetAccessEnabled":true,
	    "cellularBackupEnabled":false,"mdnsForwardingEnabled":false,
	    "ipv4":{"hostIpAddress":"198.51.100.1","prefixLength":24,"autoScaleEnabled":false,
	      "dhcp":{"mode":"SERVER","rangeStart":"198.51.100.100","rangeStop":"198.51.100.199","leaseTimeSeconds":3600}}}],
	  "wifi": [{"name":"example-main","enabled":true,"network":"NATIVE",
	    "security":{"type":"WPA2_PERSONAL","passphraseEnv":"UNIFI_WIFI_MAIN"},
	    "bands":[2.4,5],"clientIsolationEnabled":false,"hideName":false,
	    "multicastToUnicastConversionEnabled":true,"uapsdEnabled":false}],
	  "dnsPolicies": [{"type":"A_RECORD","enabled":true,"domain":"new.example.internal","ipv4Address":"192.0.2.30","ttlSeconds":0}],
	  "deletions": ["dns policy A_RECORD nas.example.internal"]
	}`
	stdout := mustRun(t, f, desired, env, "sync", "--prune", "--snapshot-dir", dir)

	if snapshotsAtFirstWrite != 1 {
		t.Fatalf("snapshots present at the first write = %d, want 1", snapshotsAtFirstWrite)
	}
	snaps := snapshots(t, dir)
	if len(snaps) != 1 || !strings.Contains(stdout, snaps[0]) {
		t.Fatalf("snapshot path not printed or wrong count: %v\n%s", snaps, stdout)
	}
	data, err := os.ReadFile(snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "super-secret-passphrase") {
		t.Fatalf("snapshot contains a passphrase:\n%s", data)
	}
	if !strings.Contains(string(data), `"passphraseEnv": "UNIFI_WIFI_MAIN"`) {
		t.Errorf("snapshot does not carry the instance file's passphraseEnv:\n%s", data)
	}
	if f.objectNamed(collNetworks, "IoT")["vlanId"] != float64(30) {
		t.Fatalf("sync did not apply the update")
	}

	// The created record is not in the snapshot, and a snapshot lists no
	// deletions, so undoing it needs --force. Without it, restore refuses.
	f.onMutation = nil
	if _, stderr, code := run(t, f, "", env, "restore", "--prune", snaps[0]); code != 1 || !strings.Contains(stderr, "new.example.internal") {
		t.Fatalf("restore --prune without --force did not refuse (exit %d):\n%s", code, stderr)
	}
	mustRun(t, f, "", env, "restore", "--prune", "--force", snaps[0])

	if after := mustRun(t, f, "", nil, "export"); after != before {
		t.Errorf("restore did not round-trip the site\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if out := mustRun(t, f, "", env, "restore", "--dry-run", snaps[0]); strings.Contains(out, "UPDATE") || strings.Contains(out, "CREATE") {
		t.Errorf("restore of the current state is not a no-op:\n%s", out)
	}
}

func TestSnapshotsAreRotated(t *testing.T) {
	f := newFakeConsole(t)
	dir := t.TempDir()
	for i := range 4 {
		desired := fmt.Sprintf(`{"dnsPolicies":[{"type":"A_RECORD","enabled":true,"domain":"h.example.internal","ipv4Address":"192.0.2.%d","ttlSeconds":0}]}`, i+1)
		mustRun(t, f, desired, nil, "sync", "--snapshot-dir", dir, "--snapshot-keep", "2")
	}
	if got := snapshots(t, dir); len(got) != 2 {
		t.Errorf("have %d snapshots, want 2: %v", len(got), got)
	}
}

// orderedPolicies declares four iot -> internal policies with the given order
// values, so a test can reorder them by changing only the numbers.
func orderedPolicies(orders [4]int) string {
	var entries []string
	for i, o := range orders {
		entries = append(entries, fmt.Sprintf(`{"name":"p%d","enabled":true,"action":"BLOCK","allowReturnTraffic":true,
		  "sourceZone":"iot","destinationZone":"internal","ipVersion":"IPV4","loggingEnabled":false,"order":%d}`, i, o))
	}
	return `{
	  "firewallZones": [{"name":"internal","networks":["Default"]},{"name":"iot","networks":["IoT"]}],
	  "firewallPolicies": [` + strings.Join(entries, ",") + `]
	}`
}

// One ordering request can rearrange a whole zone pair, so every policy it
// moves counts towards --max-changes: an oversized reorder is refused before
// the snapshot and before any write, and a small one goes through.
func TestReorderCountsTowardsMaxChanges(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	mustRun(t, f, orderedPolicies([4]int{10, 20, 30, 40}), nil, "sync")
	if want := []string{"p0", "p1", "p2", "p3"}; !equalStrings(f.policyOrder(), want) {
		t.Fatalf("policies ordered %v, want %v", f.policyOrder(), want)
	}
	dir := t.TempDir()
	writes := len(f.recorded())

	// Reversing moves all four policies; the limit is three.
	reversed := orderedPolicies([4]int{40, 30, 20, 10})
	if out, _, code := run(t, f, reversed, nil, "diff", "--max-changes", "3"); code != exitChangesPending ||
		!strings.Contains(out, "ORDER  firewall policy iot -> internal (4 moved)") ||
		!strings.Contains(out, "sync would refuse this plan") ||
		!strings.Contains(out, "4 firewall policies moved") {
		t.Errorf("diff did not report the oversized reorder (exit %d):\n%s", code, out)
	}
	stdout, stderr, code := run(t, f, reversed, nil, "sync", "--max-changes", "3", "--snapshot-dir", dir)
	if code != 1 || !strings.Contains(stderr, "--max-changes 3") || !strings.Contains(stdout, "(4 moved)") {
		t.Fatalf("guard did not refuse the reorder (exit %d)\n%s\n%s", code, stdout, stderr)
	}
	if muts := f.recorded()[writes:]; len(muts) != 0 {
		t.Errorf("a refused reorder wrote to the console: %+v", muts)
	}
	if s := snapshots(t, dir); len(s) != 0 {
		t.Errorf("a refused reorder wrote a snapshot: %v", s)
	}
	if want := []string{"p0", "p1", "p2", "p3"}; !equalStrings(f.policyOrder(), want) {
		t.Errorf("policies ordered %v after a refused sync, want %v", f.policyOrder(), want)
	}

	// Swapping the first two moves two, within a limit of two.
	stdout = mustRun(t, f, orderedPolicies([4]int{20, 10, 30, 40}), nil, "sync", "--max-changes", "2", "--snapshot-dir", dir)
	if !strings.Contains(stdout, "(2 moved)") {
		t.Errorf("plan does not show the moved count:\n%s", stdout)
	}
	if want := []string{"p1", "p0", "p2", "p3"}; !equalStrings(f.policyOrder(), want) {
		t.Errorf("policies ordered %v, want %v", f.policyOrder(), want)
	}
	if s := snapshots(t, dir); len(s) != 1 {
		t.Errorf("have %d snapshots after the allowed reorder, want 1", len(s))
	}
}

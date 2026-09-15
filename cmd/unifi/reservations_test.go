package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Reservation tests drive the legacy `rest/user` endpoints of the fake. Each
// seeds the fake's two networks (seedSite) so reservations have networks to
// be named against.

const (
	macPrinter = "02:00:5e:00:00:01"
	macTV      = "02:00:5e:00:00:02"
	macKVM     = "02:00:5e:00:00:03"
	macPhone   = "02:00:5e:00:00:04"
	macCamera  = "02:00:5e:00:00:05"
)

// legacyWrites is every non-GET request that reached the legacy API.
func legacyWrites(f *fakeConsole) []mutation {
	var out []mutation
	for _, m := range f.recorded() {
		if strings.HasPrefix(m.Path, "legacy/") {
			out = append(out, m)
		}
	}
	return out
}

func TestExportReadsReservations(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	def, iot := f.legacyNetworkIDNamed("Default"), f.legacyNetworkIDNamed("IoT")

	f.seedUser(map[string]any{"mac": macPrinter, "name": "printer", "use_fixedip": true, "fixed_ip": "192.0.2.20", "network_id": def})
	// No stored network_id: the network comes from where the client is now...
	f.seedUser(map[string]any{"mac": macTV, "name": "tv", "use_fixedip": true, "fixed_ip": "198.51.100.30", "last_connection_network_id": def})
	f.stations[macTV] = iot
	// ...or, for an offline client, where it was last seen.
	f.seedUser(map[string]any{"mac": macCamera, "use_fixedip": true, "fixed_ip": "198.51.100.40", "last_connection_network_id": iot})
	// A disabled reservation leaves fixed_ip behind; it is not a reservation.
	f.seedUser(map[string]any{"mac": macKVM, "name": "jetkvm", "use_fixedip": false, "fixed_ip": "192.0.2.152"})
	f.seedUser(map[string]any{"mac": macPhone, "name": "phone"})

	exported := mustRun(t, f, "", nil, "export")
	var doc site
	if err := json.Unmarshal([]byte(exported), &doc); err != nil {
		t.Fatal(err)
	}
	want := []reservation{
		{MAC: macPrinter, Name: "printer", FixedIP: "192.0.2.20", Network: "Default"},
		{MAC: macTV, Name: "tv", FixedIP: "198.51.100.30", Network: "IoT"},
		{MAC: macCamera, FixedIP: "198.51.100.40", Network: "IoT"},
	}
	if !reflect.DeepEqual(doc.Reservations, want) {
		t.Fatalf("exported reservations = %+v\nwant %+v", doc.Reservations, want)
	}

	stdout, _, code := run(t, f, exported, []string{"UNIFI_WIFI_EXAMPLE_MAIN=super-secret-passphrase"}, "diff", "--prune")
	if code != 0 {
		t.Fatalf("export -> diff --prune is not a no-op (exit %d):\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "OK     reservation    tv "+macTV+" -> 198.51.100.30 (IoT)") {
		t.Errorf("diff did not report the reservation:\n%s", stdout)
	}
}

func TestReservationCreate(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	def, iot := f.legacyNetworkIDNamed("Default"), f.legacyNetworkIDNamed("IoT")
	// A client the console knows, with only a leftover disabled reservation.
	kvm := f.seedUser(map[string]any{"mac": macKVM, "name": "jetkvm", "use_fixedip": false, "fixed_ip": "192.0.2.152"})

	desired := `{"reservations": [
	  {"mac":"` + strings.ToUpper(macPrinter) + `","name":"printer","fixedIp":"198.51.100.20","network":"IoT"},
	  {"mac":"` + macKVM + `","fixedIp":"192.0.2.153","network":"Default"}
	]}`
	stdout := mustRun(t, f, desired, nil, "sync")

	if !strings.Contains(stdout, "CREATE reservation    printer "+macPrinter+" -> 198.51.100.20 (IoT)") {
		t.Errorf("plan line missing:\n%s", stdout)
	}
	want := []mutation{
		{Method: "POST", Path: "legacy/rest/user", Body: map[string]any{
			"mac": macPrinter, "name": "printer", "use_fixedip": true, "fixed_ip": "198.51.100.20", "network_id": iot}},
		{Method: "PUT", Path: "legacy/rest/user/" + kvm, Body: map[string]any{
			"use_fixedip": true, "fixed_ip": "192.0.2.153", "network_id": def}},
	}
	if got := legacyWrites(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %+v\nwant %+v", got, want)
	}
	if name := f.user(kvm)["name"]; name != "jetkvm" {
		t.Errorf("a reservation without a name renamed the client to %v", name)
	}

	if stdout, _, code := run(t, f, desired, nil, "diff", "--prune"); code != 0 {
		t.Errorf("second run is not a no-op (exit %d):\n%s", code, stdout)
	}
}

func TestReservationUpdate(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	def, iot := f.legacyNetworkIDNamed("Default"), f.legacyNetworkIDNamed("IoT")

	printer := f.seedUser(map[string]any{"mac": macPrinter, "name": "printer", "use_fixedip": true, "fixed_ip": "192.0.2.20", "network_id": def})
	// No stored network_id and on Default now; declared on IoT.
	tv := f.seedUser(map[string]any{"mac": macTV, "use_fixedip": true, "fixed_ip": "198.51.100.30"})
	f.stations[macTV] = def
	// No stored network_id, and the live network agrees: nothing to do.
	f.seedUser(map[string]any{"mac": macKVM, "use_fixedip": true, "fixed_ip": "192.0.2.152"})
	f.stations[macKVM] = def
	// No stored network_id and nowhere to infer one from: set it explicitly.
	camera := f.seedUser(map[string]any{"mac": macCamera, "use_fixedip": true, "fixed_ip": "192.0.2.40"})

	desired := `{"reservations": [
	  {"mac":"` + macPrinter + `","fixedIp":"192.0.2.21","network":"Default"},
	  {"mac":"` + macTV + `","fixedIp":"198.51.100.30","network":"IoT"},
	  {"mac":"` + macKVM + `","fixedIp":"192.0.2.152","network":"Default"},
	  {"mac":"` + macCamera + `","fixedIp":"192.0.2.40","network":"Default"}
	]}`
	stdout := mustRun(t, f, desired, nil, "sync")

	for _, line := range []string{
		"UPDATE reservation    " + macPrinter + " -> 192.0.2.21 (Default; fixedIp 192.0.2.20 -> 192.0.2.21)",
		"UPDATE reservation    " + macTV + " -> 198.51.100.30 (IoT; network Default -> IoT)",
		"OK     reservation    " + macKVM + " -> 192.0.2.152 (Default)",
		"UPDATE reservation    " + macCamera + " -> 192.0.2.40 (Default; network unknown -> Default)",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("plan is missing %q:\n%s", line, stdout)
		}
	}
	want := []mutation{
		{Method: "PUT", Path: "legacy/rest/user/" + printer, Body: map[string]any{"use_fixedip": true, "fixed_ip": "192.0.2.21", "network_id": def}},
		{Method: "PUT", Path: "legacy/rest/user/" + tv, Body: map[string]any{"use_fixedip": true, "fixed_ip": "198.51.100.30", "network_id": iot}},
		{Method: "PUT", Path: "legacy/rest/user/" + camera, Body: map[string]any{"use_fixedip": true, "fixed_ip": "192.0.2.40", "network_id": def}},
	}
	if got := legacyWrites(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes = %+v\nwant %+v", got, want)
	}
	if name := f.user(printer)["name"]; name != "printer" {
		t.Errorf("an update without a name changed the client's name to %v", name)
	}
}

// TestReservationPruneClears: pruning turns use_fixedip off and keeps the
// client record. forget-sta would delete the whole client, so it must never
// be called.
func TestReservationPruneClears(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	def := f.legacyNetworkIDNamed("Default")

	f.seedUser(map[string]any{"mac": macPrinter, "name": "printer", "use_fixedip": true, "fixed_ip": "192.0.2.20", "network_id": def})
	tv := f.seedUser(map[string]any{"mac": macTV, "name": "tv", "use_fixedip": true, "fixed_ip": "192.0.2.30", "network_id": def})
	f.seedUser(map[string]any{"mac": macKVM, "use_fixedip": false, "fixed_ip": "192.0.2.152"})

	keepPrinter := `{"reservations": [{"mac":"` + macPrinter + `","name":"printer","fixedIp":"192.0.2.20","network":"Default"}]}`
	mustRun(t, f, keepPrinter, nil, "sync")
	if got := legacyWrites(f); len(got) != 0 {
		t.Fatalf("sync without --prune wrote: %+v", got)
	}

	keepPrinterDropTV := `{"reservations": [{"mac":"` + macPrinter + `","name":"printer","fixedIp":"192.0.2.20","network":"Default"}],
	  "deletions": ["reservation ` + macTV + `"]}`
	stdout := mustRun(t, f, keepPrinterDropTV, nil, "sync", "--prune")
	want := []mutation{{Method: "PUT", Path: "legacy/rest/user/" + tv, Body: map[string]any{"use_fixedip": false}}}
	if got := legacyWrites(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("prune writes = %+v\nwant %+v", got, want)
	}
	if !strings.Contains(stdout, "DELETE reservation    "+macTV+" (tv -> 192.0.2.30 on Default; listed in deletions)") ||
		!strings.Contains(stdout, "*** DELETED 1 object(s) ***") {
		t.Errorf("prune plan is not loud enough:\n%s", stdout)
	}
	if u := f.userByMAC(macTV); u == nil || u["use_fixedip"] != false || u["name"] != "tv" {
		t.Errorf("client record after prune = %v, want it kept with use_fixedip false", u)
	}

	// A declared-empty section clears every remaining reservation.
	mustRun(t, f, `{"reservations": [], "deletions": ["reservation `+macPrinter+`"]}`, nil, "sync", "--prune")
	for _, m := range legacyWrites(f) {
		if m.Method != "PUT" || m.Body["use_fixedip"] != false {
			t.Errorf("prune issued %s %s %v; it must only ever PUT use_fixedip:false", m.Method, m.Path, m.Body)
		}
	}
	for _, mac := range []string{macPrinter, macTV, macKVM} {
		if u := f.userByMAC(mac); u == nil || u["use_fixedip"] != false {
			t.Errorf("client %s after clearing = %v, want kept with use_fixedip false", mac, u)
		}
	}
}

func TestReservationsAbsentTouchesNothing(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	f.seedUser(map[string]any{"mac": macPrinter, "use_fixedip": true, "fixed_ip": "192.0.2.20"})

	mustRun(t, f, `{}`, nil, "sync", "--prune")
	if got := f.legacyRequestLog(); len(got) != 0 {
		t.Errorf("an absent reservations section reached the legacy API: %v", got)
	}
	if u := f.userByMAC(macPrinter); u["use_fixedip"] != true {
		t.Errorf("reservation after sync of an absent section = %v", u)
	}
}

func TestReservationDryRunWritesNothing(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	def := f.legacyNetworkIDNamed("Default")
	f.seedUser(map[string]any{"mac": macPrinter, "use_fixedip": true, "fixed_ip": "192.0.2.20", "network_id": def})
	f.seedUser(map[string]any{"mac": macTV, "use_fixedip": true, "fixed_ip": "192.0.2.30", "network_id": def})

	desired := `{"reservations": [
	  {"mac":"` + macPrinter + `","fixedIp":"192.0.2.21","network":"Default"},
	  {"mac":"` + macCamera + `","fixedIp":"192.0.2.40","network":"Default"}
	]}`

	stdout := mustRun(t, f, desired, nil, "sync", "--prune", "--dry-run")
	for _, verb := range []string{"CREATE reservation", "UPDATE reservation", "DELETE reservation", "would DELETE 1"} {
		if !strings.Contains(stdout, verb) {
			t.Errorf("dry-run plan is missing %q:\n%s", verb, stdout)
		}
	}
	if stdout, _, code := run(t, f, desired, nil, "diff", "--prune"); code != exitChangesPending {
		t.Errorf("diff exit = %d, want %d:\n%s", code, exitChangesPending, stdout)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Fatalf("--dry-run / diff wrote to the console: %+v", muts)
	}
}

// The guards of #18 apply to reservations: clearing one is a deletion keyed
// "reservation <mac>", and updates and clears count towards --max-changes.

// replaceRecordAndReservations declares a new DNS record (the seeded one is a
// listed deletion) and a printer reservation to create, so a plan that
// also clears the seeded tv reservation has integration and legacy writes of
// every kind. deletions is spliced in after the DNS one.
const replaceRecordAndReservations = `{
  "dnsPolicies": [{"type":"A_RECORD","enabled":true,"domain":"new.example.internal","ipv4Address":"192.0.2.30","ttlSeconds":0}],
  "reservations": [{"mac":"` + macPrinter + `","name":"printer","fixedIp":"192.0.2.20","network":"Default"}],
  "deletions": ["dns policy A_RECORD nas.example.internal"%s]
}`

func seedTVReservation(f *fakeConsole) string {
	return f.seedUser(map[string]any{"mac": macTV, "name": "tv", "use_fixedip": true,
		"fixed_ip": "192.0.2.31", "network_id": f.legacyNetworkIDNamed("Default")})
}

func TestUnlistedReservationPruneRefusesBeforeAnyWrite(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	seedTVReservation(f)
	dir := t.TempDir()
	desired := fmt.Sprintf(replaceRecordAndReservations, "")

	stdout, _, code := run(t, f, desired, nil, "diff", "--prune")
	if code != exitChangesPending ||
		!strings.Contains(stdout, "DELETE reservation    "+macTV+" (tv -> 192.0.2.31 on Default; NOT listed in deletions)") ||
		!strings.Contains(stdout, "sync would refuse this plan") {
		t.Errorf("diff did not mark the unlisted clear (exit %d):\n%s", code, stdout)
	}

	stdout, stderr, code := run(t, f, desired, nil, "sync", "--prune", "--snapshot-dir", dir)
	if code != 1 {
		t.Fatalf("sync exited %d, want 1\n%s\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, `"reservation `+macTV+`"`) {
		t.Errorf("refusal does not name the key to add:\n%s", stderr)
	}
	for _, line := range []string{"CREATE dns policy", "DELETE dns policy", "CREATE reservation", "NOT listed in deletions"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("refusal did not print %q in the plan:\n%s", line, stdout)
		}
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Errorf("a refused sync wrote to the console: %+v", muts)
	}
	for _, req := range f.legacyRequestLog() {
		if !strings.HasPrefix(req, "GET ") {
			t.Errorf("a refused sync sent a legacy write: %s", req)
		}
	}
	if s := snapshots(t, dir); len(s) != 0 {
		t.Errorf("a refused sync wrote a snapshot: %v", s)
	}
	if u := f.userByMAC(macTV); u["use_fixedip"] != true {
		t.Errorf("tv reservation after a refused sync = %v", u)
	}
}

func TestListedReservationPruneIsApplied(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	tv := seedTVReservation(f)
	dir := t.TempDir()
	const stale = "02:00:5e:00:00:99"
	desired := fmt.Sprintf(replaceRecordAndReservations, `, "reservation `+macTV+`", "reservation `+stale+`"`)

	stdout := mustRun(t, f, desired, nil, "sync", "--prune", "--snapshot-dir", dir)
	if !strings.Contains(stdout, "DELETE reservation    "+macTV+" (tv -> 192.0.2.31 on Default; listed in deletions)") {
		t.Errorf("plan does not show the listed clear:\n%s", stdout)
	}
	if !strings.Contains(stdout, `WARN   deletions      "reservation `+stale+`" matches nothing`) {
		t.Errorf("stale reservation entry was not warned about:\n%s", stdout)
	}
	if u := f.user(tv); u["use_fixedip"] != false || u["name"] != "tv" {
		t.Errorf("tv client after a listed clear = %v, want kept with use_fixedip false", u)
	}
	if u := f.userByMAC(macPrinter); u == nil || u["use_fixedip"] != true {
		t.Errorf("printer reservation was not created: %v", u)
	}
	if s := snapshots(t, dir); len(s) != 1 {
		t.Errorf("have %d snapshots, want 1", len(s))
	}
}

// A snapshot taken before the first (legacy) write holds the reservations,
// and restoring it undoes a create, an update and a clear.
func TestReservationSnapshotRestoreRoundTrip(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	def := f.legacyNetworkIDNamed("Default")
	printer := f.seedUser(map[string]any{"mac": macPrinter, "name": "printer", "use_fixedip": true, "fixed_ip": "192.0.2.20", "network_id": def})
	tv := seedTVReservation(f)
	dir := t.TempDir()
	env := []string{"UNIFI_WIFI_EXAMPLE_MAIN=super-secret-passphrase"}
	before := mustRun(t, f, "", nil, "export")

	snapshotsAtFirstWrite := -1
	f.onMutation = func(mutation) {
		if snapshotsAtFirstWrite < 0 {
			snapshotsAtFirstWrite = len(snapshots(t, dir))
		}
	}
	// Update the printer, clear tv, create the camera: only legacy writes.
	desired := `{"reservations": [
	  {"mac":"` + macPrinter + `","name":"printer","fixedIp":"192.0.2.21","network":"Default"},
	  {"mac":"` + macCamera + `","name":"camera","fixedIp":"198.51.100.40","network":"IoT"}
	], "deletions": ["reservation ` + macTV + `"]}`
	mustRun(t, f, desired, nil, "sync", "--prune", "--snapshot-dir", dir)
	f.onMutation = nil

	if snapshotsAtFirstWrite != 1 {
		t.Fatalf("snapshots present at the first legacy write = %d, want 1", snapshotsAtFirstWrite)
	}
	snaps := snapshots(t, dir)
	if len(snaps) != 1 {
		t.Fatalf("have %d snapshots, want 1", len(snaps))
	}
	data, err := os.ReadFile(snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	var snap site
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	wantSnap := []reservation{
		{MAC: macPrinter, Name: "printer", FixedIP: "192.0.2.20", Network: "Default"},
		{MAC: macTV, Name: "tv", FixedIP: "192.0.2.31", Network: "Default"},
	}
	if !reflect.DeepEqual(snap.Reservations, wantSnap) {
		t.Fatalf("snapshot reservations = %+v\nwant %+v", snap.Reservations, wantSnap)
	}
	if f.user(printer)["fixed_ip"] != "192.0.2.21" || f.user(tv)["use_fixedip"] != false {
		t.Fatalf("sync did not apply the reservation changes")
	}

	// Undoing the camera's create is a clear the snapshot does not list.
	writes := len(f.recorded())
	if _, stderr, code := run(t, f, "", env, "restore", "--prune", snaps[0]); code != 1 || !strings.Contains(stderr, `"reservation `+macCamera+`"`) {
		t.Fatalf("restore --prune without --force did not refuse (exit %d):\n%s", code, stderr)
	}
	if muts := f.recorded()[writes:]; len(muts) != 0 {
		t.Fatalf("a refused restore wrote: %+v", muts)
	}

	stdout := mustRun(t, f, "", env, "restore", "--prune", "--force", snaps[0])
	for _, line := range []string{
		"UPDATE reservation    printer " + macPrinter + " -> 192.0.2.20",
		"CREATE reservation    tv " + macTV + " -> 192.0.2.31",
		"DELETE reservation    " + macCamera + " (camera -> 198.51.100.40 on IoT; not listed in deletions; --force)",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("restore plan is missing %q:\n%s", line, stdout)
		}
	}
	if after := mustRun(t, f, "", nil, "export"); after != before {
		t.Errorf("restore did not round-trip the reservations\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if out := mustRun(t, f, "", env, "restore", "--prune", "--force", "--dry-run", snaps[0]); strings.Contains(out, "UPDATE reservation") ||
		strings.Contains(out, "CREATE reservation") || strings.Contains(out, "DELETE reservation") {
		t.Errorf("restore of the restored state is not a no-op:\n%s", out)
	}
}

func TestReservationChangesCountTowardsMaxChanges(t *testing.T) {
	f := newFakeConsole(t)
	seedSite(f)
	def := f.legacyNetworkIDNamed("Default")
	for i, mac := range []string{macPrinter, macTV, macKVM, macCamera} {
		f.seedUser(map[string]any{"mac": mac, "use_fixedip": true, "fixed_ip": fmt.Sprintf("192.0.2.%d", 20+i), "network_id": def})
	}
	// Three updates, one listed clear and one create: four guarded changes.
	desired := `{"reservations": [
	  {"mac":"` + macPrinter + `","fixedIp":"192.0.2.120","network":"Default"},
	  {"mac":"` + macTV + `","fixedIp":"192.0.2.121","network":"Default"},
	  {"mac":"` + macKVM + `","fixedIp":"192.0.2.122","network":"Default"},
	  {"mac":"` + macPhone + `","fixedIp":"192.0.2.124","network":"Default"}
	], "deletions": ["reservation ` + macCamera + `"]}`

	stdout, stderr, code := run(t, f, desired, nil, "sync", "--prune", "--max-changes", "3")
	if code != 1 || !strings.Contains(stderr, "the plan changes 4 object(s) (1 deleted, 3 updated") {
		t.Fatalf("guard did not count reservation changes (exit %d)\n%s\n%s", code, stdout, stderr)
	}
	if muts := f.recorded(); len(muts) != 0 {
		t.Fatalf("a refused sync wrote to the console: %+v", muts)
	}

	mustRun(t, f, desired, nil, "sync", "--prune", "--max-changes", "4")
	if u := f.userByMAC(macPhone); u == nil || u["use_fixedip"] != true {
		t.Errorf("sync within the limit did not create the reservation: %v", u)
	}
	if u := f.userByMAC(macCamera); u["use_fixedip"] != false {
		t.Errorf("sync within the limit did not clear the reservation: %v", u)
	}
}

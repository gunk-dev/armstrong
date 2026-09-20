# Managing a UniFi site with `cmd/unifi`

`unifi` is the sibling of the DNS tool: it reads a JSON `#Site` document on
stdin — pipe it from `cue export` — and converges a UniFi Network site to
match. It talks to the official **Integration API** served by the console for
everything except DHCP reservations and the mDNS proxy setting, which only the
legacy controller API exposes (see [DHCP reservations](#dhcp-reservations) and
[mDNS proxy](#mdns-proxy)).

The exact request and response shapes, and which of them are confirmed against
a live console, are recorded separately in
[`unifi-api-notes.md`](./unifi-api-notes.md).

## Configuration

| Variable | Meaning |
| --- | --- |
| `UNIFI_URL` | Console base URL, e.g. `https://unifi.lan` |
| `UNIFI_API_KEY` | Integration API key, from Settings → Control Plane → Integrations |
| `UNIFI_SITE` | Site name; defaults to `Default`. The legacy API addresses the same site by its `internalReference` (`default`), which `unifi` reads from `GET /sites` |
| `UNIFI_CA_FILE` | PEM bundle for the console's self-signed certificate |
| `UNIFI_INSECURE_TLS` | Set to `1` to skip certificate verification instead |
| `UNIFI_WIFI_*` | One variable per SSID, named by its `passphraseEnv` |

The console presents a self-signed certificate, so one of `UNIFI_CA_FILE` or
`UNIFI_INSECURE_TLS=1` is required. Prefer pinning the CA file.

## Commands

```sh
# Bootstrap an instance file from what the console already has.
unifi export > site.json

# Plan only. Exits 2 when anything would change, 1 on failure.
cue export ./unifi --out json -e site | unifi diff

# Converge.
cue export ./unifi --out json -e site | unifi sync --prune
```

`unifi sync --dry-run` prints exactly what `sync` would do and issues no writes;
it is the right first command to run against a console you have not synced
before.

`diff` distinguishes its two failure modes on purpose: **exit 2** means "the
plan is non-empty", **exit 1** means the command itself failed. A CI job that
gates on drift should treat only 2 as "config has drifted".

## The rules that make this safe

**Names are the identity.** Object ids are assigned by the console, so an
instance file cannot carry them and stay portable. Desired and actual objects
are matched by `name`.

Two kinds need a composite key, because their name is not unique:

| Kind | Key |
| --- | --- |
| DNS policy | `type` + `domain` (a DNS policy has no name at all) |
| Firewall policy | `sourceZone` + `destinationZone` + `name` |
| DHCP reservation | `mac` (lower-case) |

The firewall rule is not a guess. A stock UniFi Network 10.6 console ships 63
`SYSTEM_DEFINED` policies that reuse 14 names between them — `Allow All
Traffic` appears 19 times, `Block All Traffic` 16, `Allow Return Traffic` 12,
`Block Invalid Traffic` 10 — so name-keyed matching maps every duplicate onto
the same live object and the plan never converges. The triple was verified
unique across all 67 policies of the reference console, including every
system-defined one. The server-assigned `index` is *not* a usable tiebreaker:
it repeats too.

Two entries in one instance file that share a key are an error, and so is a
console that holds two policies with the same key: `unifi` reports the clash
rather than picking one.

Renaming an object in the instance file therefore reads as "delete the old one,
create a new one", not "rename" — with `--prune` that is what will happen. For
a firewall policy, moving it between zones reads the same way.

**Nothing is written back lossily.** `diff` compares only the fields the schema
models, which by itself would make a policy carrying an unmodelled filter look
like a clean no-op — while the `sync` behind it would `PUT` the policy back
stripped, turning "block one app for fifteen devices in the evening" into
"block the whole LAN".

So before planning an update to a firewall policy, `unifi` re-renders the live
object from its own projection of it and compares that against what the console
returned, key by key. If they differ, the projection is lossy and both `diff`
and `sync` fail (exit 1) naming the paths that would be lost:

```
firewall policy "iot -> external / block-the-app" uses fields schema/unifi.cue
does not model (destination.trafficFilter.webDomainFilter); managing it would
rewrite the policy without them.
```

`export` applies the same test and leaves such a policy out, reporting it on
stderr — its output is meant to feed straight back into `diff` as a no-op, and
an entry that cannot round-trip would instead become that destructive write.
The same rule covers firewall zones whose members are WAN interfaces (see
below).

**`SYSTEM_DEFINED` objects are updated but never deleted.** The console creates
its own default network, and on some setups its own firewall policies. Declaring
one in the instance file updates its configurable fields in place; `--prune`
will not remove it, so a mistake in the instance file cannot delete the LAN out
from under you.

**`--prune` only acts on resource types the instance file declares.** If
`wifi` is missing from the input entirely, no SSID is deleted — that is what
protects an instance file that simply forgot a section from wiping every
object of that type on the first sync. But a section the file *does* declare,
even as an empty list (`"wifi": []`), is fair game: every `USER_DEFINED`
object of that type is a prune candidate. **Absent = not managed, never pruned; `[]` =
managed and empty, pruned to zero.**

**Every deletion is declared.** A prune candidate is deleted only if the
instance file's `deletions` lists its key (`"wifi guest"`,
`"dns policy A_RECORD nas.example.internal"`,
`"firewall policy iot -> internal / block-cameras"`,
`"reservation 02:00:5e:10:00:11"`). Otherwise `sync` refuses
the whole run before its first write, and it does the same for a plan that
deletes, updates or moves (reorders) more than `--max-changes` objects —
DHCP reservation updates and clears and an mDNS proxy update included, creates
never. With `--snapshot-dir`, every writing run first saves the live site (DHCP
reservations and the mDNS proxy too, when the instance file declares them), and
`unifi restore` applies such a snapshot. The README's "Guards, snapshots and restore" section has the
workflow.

Every section of `#Site` is `?`-optional with no default for exactly this
reason: an instance file that omits `firewallPolicies` produces JSON with no
`firewallPolicies` key at all, and an instance file that writes
`firewallPolicies: []` produces `"firewallPolicies": []`. Before this, every
section had a default of `[...#T]`, so `cue export` filled in `[]` for a
section the instance file never mentioned — meaning an instance file that
simply hadn't gotten around to declaring, say, firewall policies would have
every one of them deleted the first time someone ran `--prune`. `cue export`
now emits a key only for a section the instance file actually set, so this
distinction is real rather than something only a hand-maintained instance
file could exploit — set a section to `[]` on purpose to clear it out, and
leave it out to leave that resource type alone.

**Secrets stay out of git.** `#WiFiSecurity` carries `passphraseEnv` — the
*name* of an environment variable — never the passphrase. `unifi export`
substitutes a generated variable name for each SSID's passphrase, so its output
is safe to commit. Passphrases and the API key are redacted from all output,
including API error responses, which can quote a rejected payload back.

**Ports are validated, not coerced.** A `#TrafficFilter`'s `portFilter.items`
take `"443"` or `"8000-8100"`. The CUE regex catches anything that is not a number
or a range; the tool additionally enforces 1-65535 and `start <= end`, which a
string regex cannot express. A port it cannot parse fails the sync rather than
being sent as 0.

**Reconciliation runs in dependency order:** the mDNS proxy's service scope (see
[mDNS proxy](#mdns-proxy)) → networks → firewall zones → wifi, firewall
policies, DNS policies and DHCP reservations. Zones reference networks
by name, and policies reference zones by name, so the ids exist by the time
they are needed.

A policy may reference a zone the instance file does not declare: zone names
are resolved against the live console, not against `firewallZones`.

## Zone-based firewall

### What a policy can say

`#FirewallPolicy` models everything the Integration API exposes on a 10.6
console:

| Field | What it is |
| --- | --- |
| `source` / `destination` | a `#TrafficFilter` narrowing that end of the policy |
| `#TrafficFilter.networkFilter` | member networks of the zone, by name |
| `#TrafficFilter.ipAddressFilter` | `IP_ADDRESS` or `SUBNET` literals |
| `#TrafficFilter.portFilter` | `#Port` strings — `"443"`, `"8000-8100"` |
| `#TrafficFilter.macAddressFilter` | lower-case MAC addresses |
| `#TrafficFilter.applicationFilter` | numeric DPI application ids |
| `schedule` | `#FirewallSchedule`: mode, time window, days, date range |
| `protocol`, `ipVersion`, `connectionStates`, `loggingEnabled` | as the API spells them |

The network, IP-address and port filters each take a `matchOpposite` flag that
inverts them; the MAC-address and application filters do not. There is no name
lookup for application ids in the Integration API, so `unifi export` is how you
find the id of an application you picked in the console UI.

`order` positions a policy among the `USER_DEFINED` policies **of its zone
pair**: the console orders policies per pair, not site-wide, and its ordering
endpoint takes the pair as query parameters. Omit `order` to leave a policy
where it is; `SYSTEM_DEFINED` policies are never reordered.

### Two limits of UniFi Network 10.6

**Policies that predate the zone-based firewall migration come back without an
`id`.** The API returns no id for them, so there is no URL to `PUT`, `PATCH`
or `DELETE` against. Policies created through the API — including by `unifi`
itself — do carry ids and are fully manageable. A plan that needs to change,
delete or reorder an id-less policy fails with a message saying so rather than
issuing a write it cannot target. Delete and re-create such a policy through
`unifi` to take ownership of it, or change it in the console UI.

**The `External` zone cannot be declared.** Its members are WAN interfaces,
which `GET /networks` does not return, so their ids cannot be turned back into
names. `export` skips the zone and says so; declaring it anyway is an error,
because the `PUT` would carry only the members the tool could name and empty
the zone. Policies can still reference `External` by name without it being
declared.

Both are recorded, with the evidence, in
[`unifi-api-notes.md`](./unifi-api-notes.md).

### When the console still runs the legacy firewall

The firewall endpoints only exist once the console has migrated off the legacy
firewall. Until then they answer `400
api.firewall.zone-based-firewall-not-configured`, and `unifi` prints

```
SKIP   firewall       zones+policies (zone-based firewall is not configured on this console)
```

then carries on with the rest of the site. Only that specific error code is
treated this way — any other firewall failure, including an expired key, is a
hard error, so a sync can never silently stop managing the firewall.

The skip counts as an unreconciled change, so `unifi diff` keeps exiting 2 for
as long as an instance file declares zones or policies the console cannot
accept. That is deliberate: declared firewall rules that are not in force are
drift, and reporting "no changes" would hide it. Drop the `firewallZones` and
`firewallPolicies` entries until the console is migrated if you want a clean
`diff`.

The read path has since been confirmed against a console with the zone-based
firewall enabled (6 zones, 67 policies): `unifi export` then `unifi diff` is a
clean no-op over all 67. DNS policy and network updates are confirmed on UniFi
Network 10.6.101; creates and the other types are inferred from the same
convention and exercised only against the test fake. Run `--dry-run` first.

## DHCP reservations

```cue
reservations: [
	{mac: "02:00:5e:10:00:10", name: "nas", fixedIp: "192.0.2.10", network: "Default"},
]
```

A `#Reservation` gives the client with that MAC address a fixed IPv4 address
on the named network. `name` is optional; when set, it is also the client's
display name in the console, and when omitted the console's name is left alone.

**Transport.** The Integration API has no reservation surface: its `/clients`
routes are `GET`-only, list only connected clients and carry no fixed-IP field.
Reservations are therefore read and written through the console's legacy
controller API, under `/proxy/network/api/s/<site>/`, with the same
`X-API-KEY`. This is the only object type on that API. It is undocumented and
can change without notice; the code that speaks it is confined to
`cmd/unifi/legacy.go`.

| Request | Used for |
| --- | --- |
| `GET rest/user` | every client the console knows, connected or not |
| `GET rest/networkconf` | legacy network `_id` → name |
| `GET stat/sta` | the network each connected client is on now |
| `POST rest/user` `{mac, name?, use_fixedip: true, fixed_ip, network_id}` | reserve for a MAC the console has never seen |
| `PUT rest/user/{_id}` `{use_fixedip: true, fixed_ip, network_id, name?}` | reserve for a known client, or change a reservation |
| `PUT rest/user/{_id}` `{use_fixedip: false}` | clear a reservation (`--prune`) |

**What counts as a reservation.** A `rest/user` entry holds a reservation only
when `use_fixedip` is `true`. The console keeps `fixed_ip` after a reservation
is switched off, so an entry with a `fixed_ip` and `use_fixedip: false` is
treated as having no reservation, by `export` and by the plan alike.

**Networks are joined by name.** The legacy API's network ids do not match the
Integration API's, so `network` is resolved through `rest/networkconf`. An
entry may also store no `network_id` at all, which the console reads as "the
client's current network". `unifi` then takes the network from `stat/sta` if
the client is connected, or from the entry's last-seen network if not. When
that matches the declared `network`, the reservation is `OK`; otherwise the
plan updates it and sets `network_id` explicitly.

**Plan lines** name the reservation, its address and its network:

```
CREATE reservation    nas 02:00:5e:10:00:10 -> 192.0.2.10 (Default)
UPDATE reservation    nas 02:00:5e:10:00:10 -> 192.0.2.11 (Default; fixedIp 192.0.2.10 -> 192.0.2.11)
OK     reservation    nas 02:00:5e:10:00:10 -> 192.0.2.10 (Default)
DELETE reservation    02:00:5e:10:00:11 (tv -> 192.0.2.30 on Default; listed in deletions)
```

A `DELETE` line leads with the bare MAC because that is the key `deletions`
lists: `"reservation 02:00:5e:10:00:11"`.

**Pruning clears, never forgets.** With `--prune` and a declared
`reservations` section, a reservation the instance file does not list is
cleared with `PUT use_fixedip: false` — if `deletions` lists
`reservation <mac>`; otherwise `sync` refuses the run, as for any other
unlisted deletion. The client record, with its name and
history, stays. `unifi` never calls `cmd/stamgr forget-sta`, which would delete
the whole client. As for every other section, an absent `reservations` key is
not managed (the legacy API is not even contacted), and `reservations: []`
with `--prune` clears every reservation on the site.

`export` emits every reservation with its network named, so
`unifi export | unifi diff` is a no-op. A reservation that stores no network
for a client never seen on one cannot be named; `export` leaves it out and
says so on stderr.

## mDNS proxy

```cue
mdns: {
	mode: "custom"
	services: ["apple_airPlay", "google_chromecast", "printers"]
	customServices: [{name: "HomeKit", address: "_hap._tcp"}]
}
```

The gateway runs **one** mDNS proxy for the whole site. A network takes part
when its `mdnsForwardingEnabled` is true, and that flag is the whole story for
participation: every participating network shares one service scope, and the
proxy is off when no network participates. Enabling it on four networks
therefore reflects every allowed announcement between all four, across tiers
the firewall keeps apart. `#Site.mdns` is the service scope:

| `mode` | Effect |
| --- | --- |
| `all` | every service the gateway knows crosses (the console UI's Auto) |
| `custom` | only `services` (predefined codes) and `customServices` cross; at least one is required |

A custom service is `{name, address}`: `address` is `_service._tcp` or
`_service._udp`, `name` the label the console shows. Nothing partitions the
scope pairwise, so "People ↔ IoT, Guest ↔ Media only" cannot be expressed.
Omitting `mdns` leaves the service scope unmanaged — the legacy API is not
asked for it.

**Transport.** The Integration API exposes only the per-network flag, which
`unifi` writes with the network. The service scope goes through the legacy
controller API, like reservations, with the same key and in
`cmd/unifi/legacy.go`:

| Request | Used for |
| --- | --- |
| `GET rest/setting` | every settings object; the scope is the one with `key: "mdns"` |
| `POST set/setting/mdns` | `mode`, `predefined_services`, `custom_services`, and `enabled_for` / `enabled_for_network_ids` echoed as read (the console requires all five and ignores the last two) |

Outside `custom` both service lists are sent empty, as the console UI does. A
console that reports the legacy `mode: "auto"` with its whole catalogue reads
as `all`.

**Plan lines** name the desired mode:

```
OK     mdns proxy     all
OK     mdns proxy     custom (services apple_airPlay, google_chromecast; customServices HomeKit (_hap._tcp))
UPDATE mdns proxy     custom (mode all -> custom; services +apple_airPlay +printers)
UPDATE mdns proxy     custom (services -ssh_servers -web_servers; customServices +HomeKit (_hap._tcp))
UPDATE mdns proxy     all (mode custom -> all)
```

Services compare as sets and custom services as sets of `(name, address)`
pairs: reordering them is not a change.

**A singleton.** The setting always exists, so it is only ever updated, never
created or deleted: `--prune` and `deletions` do not apply. An update counts
towards `--max-changes`. It is reconciled before networks, so the scope is in
force before a network in the same run starts to participate. A run that
declares `mdns` snapshots it under `--snapshot-dir`, so `restore` puts it back;
a run that does not leaves it out of the snapshot, and restoring that snapshot
leaves the scope alone. `restore` refuses an `mdns` holding keys `#MDNS` does
not have, such as a `networks` list.

**Fail closed.** `unifi` never plans over a live setting it cannot read with
certainty: a field of an unexpected type, an unknown `mode` or `enabled_for`,
or a `custom_services` entry of another shape (in any mode, since a write
replaces them). `diff` and `sync` exit 1 before any write, and `export` leaves
`mdns` out and says why on stderr.

**Service codes are not checked locally.** `services` takes any non-empty
string. The console rejects an unknown code and `unifi` reports its
`api.err.*` message. The codes seen on 10.6.101 are listed in
[`unifi-api-notes.md`](./unifi-api-notes.md).

## Running it

`unifi sync` belongs on a **LAN host**, not in GitHub Actions: the console is
reachable only on the local network and presents a certificate no public runner
will trust. The intended deployment is a systemd timer on a NixOS box:

```
ExecStart = "${pkgs.writeShellScript "unifi-sync" ''
  ${cue}/bin/cue export ${./unifi} --out json -e site | ${armstrong}/bin/unifi sync --prune
''}";
```

with the API key and SSID passphrases supplied through
`systemd`'s `LoadCredential=` or an `EnvironmentFile=` outside the store.

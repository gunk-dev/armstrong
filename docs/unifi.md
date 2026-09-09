# Managing a UniFi site with `cmd/unifi`

`unifi` is the sibling of the DNS tool: it reads a JSON `#Site` document on
stdin — pipe it from `cue export` — and converges a UniFi Network site to
match. It talks to the official **Integration API** served by the console, not
the private controller API.

The exact request and response shapes, and which of them are confirmed against
a live console, are recorded separately in
[`unifi-api-notes.md`](./unifi-api-notes.md).

## Configuration

| Variable | Meaning |
| --- | --- |
| `UNIFI_URL` | Console base URL, e.g. `https://unifi.lan` |
| `UNIFI_API_KEY` | Integration API key, from Settings → Control Plane → Integrations |
| `UNIFI_SITE` | Site name; defaults to `Default` |
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
object of that type is deleted. `cue export` always emits every key, so a
hand-maintained instance file is the only place this distinction matters —
omit a key to leave a resource type alone, or set it to `[]` to clear it out.

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

**Reconciliation runs in dependency order:** networks → firewall zones → wifi,
firewall policies and DNS policies. Zones reference networks by name, and
policies reference zones by name, so the ids exist by the time they are needed.

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

**`USER_DEFINED` policies come back without an `id`.** The API returns one for
every system-defined policy and none for any user-created one, so there is no
URL to `PUT`, `PATCH` or `DELETE` against. `unifi` can create policies and can
update the system-defined ones; a plan that needs to change, delete or reorder
a user-created policy fails with a message saying so rather than issuing a
write it cannot target. Delete and re-create such a policy through `unifi` to
take ownership of it, or change it in the console UI.

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
clean no-op over all 67. **No write has been made against a live console**, so
create/update payloads remain inferred and are exercised only against the test
fake. Run `--dry-run` first.

## DHCP reservations are not modelled

A client's fixed IP cannot be managed through the Integration API on Network
10.6: `/sites/{id}/clients` and `/sites/{id}/clients/{id}` are both `GET`-only,
the client object has no fixed-IP field even for a client that holds a
reservation, and only currently-connected clients are listed at all — so a
reservation held by MAC for a device that is switched off is not even readable.
No reservation-shaped endpoint exists elsewhere under `/integration/v1`.

Reservations do live on the console, but on the private controller API, which
this tool deliberately does not speak. `#Client` is therefore absent from
`schema/unifi.cue` rather than present and inert. The full evidence is in
[`unifi-api-notes.md`](./unifi-api-notes.md#clients--no-dhcp-reservation-write-path-issue-13).

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

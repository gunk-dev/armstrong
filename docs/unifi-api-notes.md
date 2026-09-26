# UniFi Network Integration API — discovery notes

Field names and response shapes below were read from a live UniFi Dream Machine
Pro running **UniFi Network 10.6.101** (UniFi OS 3.0.1) on 2026-09-07, and
re-read on 2026-09-09 with the zone-based firewall enabled (6 zones, 67
policies). No addresses, ids or keys are recorded here — only the schema.

The console also serves interactive docs for its own version at
Settings → Control Plane → Integrations (`/settings/api-docs` in the UniFi OS
UI). There is no machine-readable OpenAPI document at a stable URL: every
`*/openapi.json` and `*/api-docs*` path returns the SPA shell or 404, so the
shapes here were derived from live `GET`s.

## Transport

- Base: `https://<console>/proxy/network/integration/v1`
- Auth: `X-API-KEY: <key>` header. A bad key returns **401**
  `{"error":{"code":401,"message":"Unauthorized"}}` — note this envelope differs
  from the one used for application-level errors (below).
- The console presents a self-signed certificate.
- `GET /info` → `{"applicationVersion":"10.6.101"}` — useful as a reachability check.
- `GET /sites` → `{"data":[{"id","internalReference","name"}], …}`. Everything
  else lives under `/sites/{siteId}/`.

### Paging

List endpoints answer `{"data":[…],"offset","limit","count","totalCount"}`.
The default `limit` is **25** and the server **caps it at 200**: asking for
`limit=1000` echoes back `limit: 200`. So a client must page — `?limit=200` plus
`offset` — and cannot avoid it by asking for a large page.

### Application errors

Non-auth failures use a different envelope:

```json
{"statusCode":400,"statusName":"BAD_REQUEST",
 "code":"api.firewall.zone-based-firewall-not-configured",
 "message":"Zone Based Firewall is not configured",
 "timestamp":"…","requestPath":"…","requestId":"…"}
```

The machine-readable `code` is what to branch on, not the message.

## Allowed methods

Read off the `Allow` header returned with a `405` (send an unsupported method —
this is safe, nothing is mutated):

| Path (under `/sites/{id}`) | Methods |
| --- | --- |
| `/networks` | GET, POST |
| `/networks/{id}` | GET, PUT, DELETE |
| `/wifi/broadcasts` | GET, POST |
| `/wifi/broadcasts/{id}` | GET, PUT, DELETE |
| `/dns/policies` | GET, POST |
| `/dns/policies/{id}` | GET, PUT, DELETE |
| `/firewall/zones` | GET, POST |
| `/firewall/zones/{id}` | GET, PUT, DELETE |
| `/firewall/policies` | GET, POST |
| `/firewall/policies/{id}` | GET, PUT, PATCH, DELETE |
| `/firewall/policies/ordering` | GET, PUT, PATCH, DELETE |
| `/acl-rules` | GET, POST |
| `/devices` | GET, POST |
| `/clients` | **GET only** |
| `/clients/{id}` | **GET only** |
| `/info`, `/sites` | GET |

Read the `Allow` header, not the CORS preflight: `OPTIONS` on *any* path answers
`204` with a blanket
`access-control-allow-methods: HEAD, GET, DELETE, PATCH, POST, PUT`, which is
nginx boilerplate and says nothing about the route. The `405` `Allow` header is
the route's real method set.

## Shapes (confirmed live)

`metadata` appears on every configurable object:
`{"origin":"SYSTEM_DEFINED"|"USER_DEFINED","configurable":bool}`. `configurable`
is present on networks but not on wifi/dns objects.

### Networks

**The list response is an overview and omits the addressing.** `GET /networks`
returns only `{id,name,management,enabled,vlanId,default,metadata}`; you must
`GET /networks/{id}` for `ipv4Configuration`. The tool therefore does one detail
fetch per network.

`GET /networks/{id}`:

```
id, name, management: "GATEWAY", enabled, vlanId, default,
metadata: {origin, configurable},
isolationEnabled, internetAccessEnabled, cellularBackupEnabled, mdnsForwardingEnabled,
zoneId,                                  // with the zone-based firewall
ipv4Configuration: {
  hostIpAddress, prefixLength, autoScaleEnabled,
  dhcpConfiguration: {
    mode: "SERVER",
    ipAddressRange: {start, stop},
    leaseTimeSeconds, domainName, pingConflictDetectionEnabled,
    dnsServerIpAddressesOverride: [string],
    pxeConfiguration: {filename, serverIpAddress}
  }
}
```

Note the nesting names: `ipv4Configuration` (not `ipv4`) and
`dhcpConfiguration` (not `dhcp`), and the DHCP pool is an
`ipAddressRange: {start, stop}` object rather than two flat fields. The schema
in this repo flattens these for readability; `cmd/unifi/api.go` translates.

**On a console with the zone-based firewall, `POST /networks` requires
`zoneId`.** Confirmed on a UDM Pro running UniFi Network 10.6.106: a
create body without it is refused before anything is created.

```
POST /sites/{siteId}/networks
{"management":"GATEWAY","name":"People","enabled":true,"vlanId":10,
 "isolationEnabled":false,"internetAccessEnabled":true,
 "cellularBackupEnabled":false,"mdnsForwardingEnabled":true,
 "ipv4Configuration":{...}}

400 {"statusCode":400,"statusName":"BAD_REQUEST",
     "code":"api.network.validation.missing-zone-id",
     "message":"zoneId must not be null", ...}
```

The create body carries `"zoneId": "<firewall zone id>"`, and the network is
created as a member of that zone: it appears in that zone's `networkIds`. A
network belongs to exactly one zone. `cmd/unifi` therefore creates a declared
zone the console lacks before the networks that join it, then creates each
network with its zone's id; `cmd/unifi/fake_test.go` and the VM test's
`fake-console.py` both refuse a zoneless create with the same code.

`pxeConfiguration` is not modelled by `schema/unifi.cue` — nothing here needs
PXE, and the tool never sends the key, so the console keeps whatever is set.

### WiFi broadcasts

Same overview/detail split: the list omits the tuning flags and the passphrase.

`GET /wifi/broadcasts/{id}`:

```
id, type: "STANDARD", name, enabled, metadata: {origin},
network: {type: "NATIVE"}        // or {type: "SPECIFIC", networkId}
securityConfiguration: {type: "WPA2_PERSONAL", passphrase, fastRoamingEnabled}
broadcastingFrequenciesGHz: [2.4, 5]   // JSON numbers, so 2.4 is a float
clientIsolationEnabled, hideName, multicastToUnicastConversionEnabled,
uapsdEnabled, bandSteeringEnabled, arpProxyEnabled, bssTransitionEnabled,
advertiseDeviceName, channel2gLockedTo6, dtimPeriod2gLockedTo3
```

**`passphrase` comes back in plaintext on GET.** Never log a raw wifi response.

`channel2gLockedTo6` and `dtimPeriod2gLockedTo3` are read-only compatibility
flags; they are not modelled.

### DNS policies

Flat objects, no overview/detail split:

```
id, type: "A_RECORD", enabled, domain, ipv4Address, ttlSeconds, metadata: {origin}
```

The other record types (`AAAA_RECORD`, `CNAME_RECORD`, `MX_RECORD`,
`TXT_RECORD`, `SRV_RECORD`, `FORWARD_DOMAIN`) swap `ipv4Address` for the
type-specific payload field; only `A_RECORD` was present on the reference
console. **DNS policies have no name**, so the tool keys them by
`type + domain`.

### Firewall zones

```
{id, name, networkIds: [string], metadata: {origin, configurable}}
```

`networkIds` can name networks `GET /networks` does not return. On the
reference console the `External` zone lists two WAN interfaces, and neither has
a `/networks` entry, so their ids cannot be turned back into names. Those zones
are also `configurable: false`. `unifi export` therefore skips a zone whose
members it cannot name, and says so on stderr — emitting `networks: ["", ""]`
would round-trip into a `PUT` that empties the zone.

Zones need not be declared to be *referenced*: `unifi` resolves policy zone
names against the live zone list, so a policy can point at `External` while the
instance file declares no zones at all.

### Firewall policies

`GET /firewall/policies` returns whole objects (no overview/detail split) in
**evaluation order**. Confirmed against a console with the zone-based firewall
enabled: 6 zones, 67 policies (63 `SYSTEM_DEFINED`, 4 `USER_DEFINED`).

```
id, name, description, enabled, index,
metadata: {origin, configurable},
action: {type: "ALLOW"|"BLOCK"|"REJECT", allowReturnTraffic},   // allowReturnTraffic only on ALLOW
source|destination: {
  zoneId,
  trafficFilter: {
    type: "NETWORK"|"IP_ADDRESS"|"PORT"|"MAC_ADDRESS"|"APPLICATION",
    networkFilter:     {networkIds: [id], matchOpposite},
    ipAddressFilter:   {type: "IP_ADDRESSES", matchOpposite,
                        items: [{type: "IP_ADDRESS"|"SUBNET", value: "224.0.0.251"|"fe80::/10"}]},
    portFilter:        {type: "PORTS", matchOpposite,
                        items: [{type: "PORT_NUMBER", value: 5353}]},
    macAddressFilter:  {macAddresses: ["aa:bb:cc:dd:ee:ff", …]},
    applicationFilter: {applicationIds: [262392, …]}
  }
},
ipProtocolScope: {ipVersion: "IPV4"|"IPV6"|"IPV4_AND_IPV6",
                  protocolFilter: {type: "NAMED_PROTOCOL", protocol: {name: "UDP"}, matchOpposite}
                                | {type: "PRESET", preset: {name: "TCP_UDP"}}},
connectionStateFilter: ["NEW"|"INVALID"|"ESTABLISHED"|"RELATED"],
loggingEnabled,
schedule: {mode: "EVERY_DAY"|"CUSTOM"|…,
           timeFilter: {startTime: "22:00", stopTime: "13:00"},
           repeatOnDays: ["MONDAY", …], startDate: "2026-03-14", stopDate: "2026-03-21"}
```

Corrections to what this file previously guessed:

- port items carry **`value`**, not `port` / `startPort` / `endPort`;
- a single protocol is `{type: "NAMED_PROTOCOL", protocol: {name}}`, not
  `{type: "NAMED", name}`, and the name is upper-case (`UDP`, `ICMPV6`);
  TCP-or-UDP is a different shape, a `PRESET` (see below);
- there is an `ipAddressFilter` and an `applicationFilter` and a
  `macAddressFilter`, and both endpoints of one policy can carry several
  filters at once (`Allow mDNS` matches an IP set *and* a port);
- every policy has a server-assigned **`index`** (evaluation order). The
  observed values were `10000`+ for user policies and `30000`+ /
  `2147483647` for system ones, and they are **not** unique.

`PORT_NUMBER_RANGE` was not present on the console. `cmd/unifi` renders a
`"8000-8100"` range as `{"type":"PORT_NUMBER_RANGE","value":"8000-8100"}` by
analogy with `PORT_NUMBER`; that is still **inferred**.

#### The protocol filter is a NAMED_PROTOCOL or a PRESET

A live `GET /firewall/policies` on 10.6.106 (168 policies) shows two shapes of
`ipProtocolScope.protocolFilter`:

```
{"type":"NAMED_PROTOCOL","protocol":{"name":"UDP"},"matchOpposite":false}   // also TCP, ICMP, ICMPV6
{"type":"PRESET","preset":{"name":"TCP_UDP"}}                                // SYSTEM_DEFINED "Allow DNS", "Allow Public DNS"
```

`TCP_UDP` is a preset, not a named protocol, and the PRESET objects carry no
`matchOpposite`. `TCP_UDP` is the only preset seen. Sending it as a named
protocol answers:

```
400 {"code":"api.request.unknown-type-id",
     "message":"Invalid $.ipProtocolScope.protocolFilter.type value 'TCP_UDP' (valid values: '')"}
```

A PRESET cannot be negated. One bounded probe (a disabled BLOCK policy
`Cameras -> Media`, no traffic filters) POSTed
`{"type":"PRESET","preset":{"name":"TCP_UDP"},"matchOpposite":true}` and got:

```
400 {"code":"api.request.unknown-property",
     "message":"Unknown request body property '$.ipProtocolScope.protocolFilter.matchOpposite'"}
```

Nothing was created. `cmd/unifi` sends `protocol: "TCP_UDP"` as the PRESET and
reads a PRESET back as `protocol: <preset name>`. `schema/unifi.cue` rejects
`protocol: "TCP_UDP"` with `protocolMatchOpposite: true`, and `cmd/unifi`
refuses it before any write. To block everything except TCP and UDP, declare
one policy per other protocol (`ICMP`, `ICMPV6`, …) instead.

#### Names are not unique; (source zone, destination zone, name) is

The console's own defaults reuse 14 names across the 67 policies — `Allow All
Traffic` ×19, `Block All Traffic` ×16, `Allow Return Traffic` ×12, `Block
Invalid Traffic` ×10. The triple **(source zone name, destination zone name,
policy name)** was verified unique across all 67, so that is the identity
`cmd/unifi` matches on. `index` is not usable as a tiebreaker: it repeats.

#### Policies that predate the zone-based firewall migration have no `id`

Four of the 67 policies on 10.6.106 come back with no `id` — the ones the
zone-based firewall migration converted from the old rule set. The ordering
endpoint agrees: `beforeSystemDefined` is `[null, null, null, null]` for their
zone pair. There is **no way to address such a policy** for `PUT`, `PATCH`,
`DELETE` or reordering through the Integration API.

Policies created through the Integration API do carry an `id` and are fully
manageable. A `POST /sites/{id}/firewall/policies` with a minimal body (name,
`enabled: false`, `action: BLOCK`, source and destination `zoneId` only,
`ipProtocolScope: IPV4_AND_IPV6`, `loggingEnabled: false`) answers 201 with an
`id` (a UUID) and an `index` of `10000`; the subsequent `GET
/firewall/policies` lists it with that id; the ordering call for its zone pair
returns `beforeSystemDefined: ["<that id>"]` rather than null; `PUT
/firewall/policies/{id}` answers 200 and echoes the change back, and `DELETE
/firewall/policies/{id}` answers 200 and drops it from the list. Policies
created in the console after the migration behave the same way.

> **Note — the v2 endpoint has ids for all of them.** The console's own
> `GET /proxy/network/v2/api/site/default/firewall-policies` (same
> `X-API-KEY`, no envelope, a bare JSON array) gives every policy a Mongo
> ObjectId `_id`, including the four migrated ones, which share the ObjectId
> time prefix of the zones themselves. That id space is disjoint from the
> Integration API's UUIDs: an Integration `GET /firewall/policies/{v2 _id}`
> answers 400. `cmd/unifi` does not use the v2 endpoint.

`cmd/unifi` can create policies, and can update the system-defined ones and
any it created itself. A plan that needs to update, delete or reorder an
id-less policy fails with a message naming this limitation, rather than
issuing a write it cannot target; deleting and re-creating such a policy
through `cmd/unifi` gives it an id.

#### Ordering is per zone pair

`GET /firewall/policies/ordering` without arguments answers `400
api.request.error` — "Required request parameter `sourceFirewallZoneId` for
method parameter type UUID is not present". It takes
`?sourceFirewallZoneId=…&destinationFirewallZoneId=…` and returns
`{orderedFirewallPolicyIds: {beforeSystemDefined: [id], afterSystemDefined: [id]}}`
for that pair alone. A `PUT` therefore has to carry the same query parameters;
there is no site-wide ordering call.

### Clients — no DHCP reservation surface (issue #13)

`GET /sites/{id}/clients` returns **only currently connected** clients, and
only these fields:

```
{id, type: "WIRED"|"WIRELESS", name, macAddress, ipAddress,
 connectedAt, uplinkDeviceId, access: {type: "DEFAULT"}}
```

`GET /clients/{id}` returns exactly the same object — there is no detail view
with more in it. Three independent findings, all from the reference console:

1. **The routes are read-only.** A `405` from `/clients` and from
   `/clients/{id}` both answer `allow: GET`.
2. **The object has no fixed-IP field.** A client that *does* hold a DHCP
   reservation (`use_fixedip: true` with a `fixed_ip` on the private controller
   API) appears in the Integration API list with the eight fields above and
   nothing more — the reservation is invisible, not merely unset.
3. **Disconnected devices are absent.** The console knew 110 clients; the
   Integration API listed the 30 that were connected. A reservation held by
   MAC for a device that is currently off cannot even be read.

No reservation-shaped endpoint exists either: `/clients/reservations`,
`/client-reservations`, `/dhcp`, `/dhcp/reservations`, `/fixed-ip-assignments`,
`/static-leases`, `/users` and `/user-groups` all answer `404`
(`/clients/<x>` answers `400 api.request.argument-type-mismatch`, i.e. it is
parsing `<x>` as a client id). `/acl-rules` exists but is a different feature
and was empty. There is no `/integration/v2`.

Reservations live on the legacy controller API instead, which is where
`cmd/unifi` manages them — see the next section.

### Legacy controller API — DHCP reservations (issue #13)

Base `https://<console>/proxy/network/api/s/<site>/`, where `<site>` is the
Integration API site's `internalReference` (`default`). The same `X-API-KEY`
authenticates reads and writes; no session cookie or CSRF token is needed.
Without the key, `401`. Every response is wrapped as
`{"meta":{"rc":"ok"|"error","msg"?},"data":[…]}`, and there is no paging.

- `GET rest/user` lists every client the console knows, offline ones included
  (112 on the reference console, against 29 connected). Entries carry `_id`,
  `mac`, `name`, `hostname`, `use_fixedip`, `fixed_ip`, `network_id`,
  `last_connection_network_id`, `last_ip`, `local_dns_record`, … An unset
  field is absent, not `null`.
- `use_fixedip` is what makes a reservation. The console keeps `fixed_ip` after
  a reservation is switched off, so `use_fixedip:false` with a `fixed_ip` is
  seen for real and is not a reservation.
- `network_id` is stored only when set explicitly. A reservation without one
  applies on the client's current network; `stat/sta` reports that as
  `network_id` for connected clients, and `rest/user` has
  `last_connection_network_id`.
- Network ids do not match between the APIs: `rest/networkconf` `_id` is a
  Mongo ObjectId, the Integration API network `id` a UUID. Name is the join.

Writes, confirmed on 10.6 with a throwaway locally-administered MAC:

| Request | Response |
| --- | --- |
| `POST rest/user` `{mac, name, use_fixedip:true, fixed_ip, network_id}` for an unknown MAC | `200`, `rc:"ok"`, the new entry with its `_id` |
| `GET rest/user/{_id}` | `200`, the entry as written |
| `PUT rest/user/{_id}` `{use_fixedip:false}` | `200`, `use_fixedip:false`; `fixed_ip`, `network_id` and `name` unchanged |
| `POST cmd/stamgr` `{cmd:"forget-sta", macs:[…]}` | `200`; the entry is gone from `rest/user` |

`PUT` merges into the entry rather than replacing it, so a body carrying only
the reservation fields leaves everything else on the client alone.
`forget-sta` deletes the whole client record, not just its reservation, which
is why `cmd/unifi` never calls it.

### Legacy controller API — mDNS proxy setting (issue #25)

Confirmed on UniFi Network 10.6.101 on 2026-09-19, by changing the setting in
the console UI (Settings → Networks → Gateway mDNS Proxy) with the network
inspector open and reproducing every write with `curl` and the same
`X-API-KEY`.

**Participation is one flag per network**, visible and writable in three
places that the console keeps in step in both directions:

| View | Field | Write |
| --- | --- | --- |
| Integration API `GET/PUT /networks/{id}` | `mdnsForwardingEnabled: bool` | `PUT` the detail body; the other views follow |
| legacy `rest/networkconf` | `mdns_enabled: bool` | not used |
| v2 global network config (below) | `mdns_enabled_for`, `mdns_enabled_for_network_ids` | partial-body `PUT` |

The enum is derived from the set of flags: all true → `all`, none true →
`none`, otherwise `some` with the true ones listed. The UI's Auto/Off/Custom
"VLAN Scope" is a bulk setter for the flags and nothing more; its Off is every
flag false. `cmd/unifi` writes participation only through the network `PUT`.

**The service scope** is the legacy settings object with `key: "mdns"`, read
from `GET rest/setting` (the list of every settings object):

| Field | Observed |
| --- | --- |
| `mode` | `all` (the UI's Auto), `custom` (the UI's Specific), or `auto` |
| `predefined_services` | `[{"code": …}]`: the allow-list in `custom`; `[]` after the UI writes `all` |
| `custom_services` | `[{"address": "_hap._tcp", "name": "HomeKit test"}]`: `address` is `_service._proto`, `name` the UI's "Label"; the UI requires both |
| `enabled_for` | `none`, `all` or `some`: a read-only projection of the per-network flags |
| `enabled_for_network_ids` | the `rest/networkconf` `_id`s (ObjectIds, not Integration API UUIDs) of the participating networks when `some` |
| `site_id`, `_id`, `key` | present |

`auto` is accepted on write but the UI never sends it; a console not written
since its migration to 10.6 reports `mode: "auto"` with the whole catalogue in
`predefined_services`, and becomes `all` with `[]` at the first UI write.
`cmd/unifi` reads `auto` as `all` and never writes it. `off` and any other
mode are refused with `api.err.InvalidPayload`. Whether `custom` with both
lists empty is accepted has not been tested; `cmd/unifi` refuses it at plan
time.

**Write:** `POST /proxy/network/api/s/default/set/setting/mdns` with a JSON body
carrying all five of `mode`, `predefined_services`, `custom_services`,
`enabled_for` and `enabled_for_network_ids`. A body with only `mode` is refused
with `api.err.InvalidPayload`. Values sent for `enabled_for*` are ignored (rc
ok, unchanged), so `cmd/unifi` echoes them as read. `key`, `site_id` and `_id`
are accepted and harmless. The response is
`{"meta":{"rc":"ok"},"data":[<the object as now stored>]}`; a refused write is
`{"meta":{"rc":"error","msg":"api.err.InvalidPayload"}}` or
`api.err.InvalidValue`. The UI sends both lists empty outside `custom`, and so
does `cmd/unifi`. `PUT rest/setting/mdns/{_id}` with the same body was also
observed to return 200 and apply; the UI uses the `POST`, and so does
`cmd/unifi`.

The 24 predefined codes (the catalogue a console reports in `auto`):
`amazon_devices`, `android_tv_remote`, `apple_airDrop`, `apple_airPlay`,
`apple_file_sharing`, `apple_iChat`, `apple_iTunes`, `aqara`, `bose`,
`dns_service_discovery`, `ftp_servers`, `google_chromecast`, `homeKit`,
`matter_network`, `philips_hue`, `printers`, `roku`, `scanners`, `sonos`,
`spotify_connect`, `ssh_servers`, `time_capsule`, `web_servers`,
`windows_file_sharing_samba`.

### v2 API — global network config

`GET/PUT /proxy/network/v2/api/site/default/global/config/network` holds,
among other site-wide network settings, `mdns_enabled_for:
"none"|"all"|"some"` and `mdns_enabled_for_network_ids: [networkconf _id]`.
A `PUT` with a partial body merges into the object; the enum refuses any other
string with a 400. It is a third view of the per-network flags, in both
directions: `some` with `[<id>]` sets that network's `mdnsForwardingEnabled`,
`some` with `[]` or `none` clears every flag, `all` sets every flag, and a
network `PUT` made afterwards re-derives the enum from the flags (`some` + `[]`
followed by one flag set true reads `all` on a one-network console).
`cmd/unifi` does not use it: the network `PUT` it already makes writes the
same state, and a second writer would model participation twice.

## Write bodies

Create/update payloads mirror the `GET` detail representation with the
server-owned keys (`id`, `metadata`, `index`, `default`) removed; that is the
shape `cmd/unifi/api.go` sends. Two no-op `PUT`s built that way returned 200
and left the object unchanged on UniFi Network 10.6.101: `/dns/policies/{id}`
(an A record) and `/networks/{id}` (Default, whose detail carries `zoneId`
under the zone-based firewall; `cmd/unifi` sends it back as read, so an update
cannot detach a network from its zone). Creates and updates of the other
types are inferred from the same convention and exercised only against the
test fake. Run `unifi sync --dry-run` first on a real deployment.

For firewall policies the inference is now checked in both directions:
`cmd/unifi` re-renders every live policy from its own projection and compares
that against the object the console returned. If they differ — because the
policy uses something `schema/unifi.cue` does not model — the policy is refused
rather than written back lossily. That check is what keeps "the write body
mirrors the read body" from being an assumption.

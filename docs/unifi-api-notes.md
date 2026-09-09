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
                  protocolFilter: {type: "NAMED_PROTOCOL", protocol: {name: "UDP"}, matchOpposite}},
connectionStateFilter: ["NEW"|"INVALID"|"ESTABLISHED"|"RELATED"],
loggingEnabled,
schedule: {mode: "EVERY_DAY"|"CUSTOM"|…,
           timeFilter: {startTime: "22:00", stopTime: "13:00"},
           repeatOnDays: ["MONDAY", …], startDate: "2026-03-14", stopDate: "2026-03-21"}
```

Corrections to what this file previously guessed:

- port items carry **`value`**, not `port` / `startPort` / `endPort`;
- the protocol filter is `{type: "NAMED_PROTOCOL", protocol: {name}}`, not
  `{type: "NAMED", name}`, and the name is upper-case (`UDP`, `ICMPV6`);
- there is an `ipAddressFilter` and an `applicationFilter` and a
  `macAddressFilter`, and both endpoints of one policy can carry several
  filters at once (`Allow mDNS` matches an IP set *and* a port);
- every policy has a server-assigned **`index`** (evaluation order). The
  observed values were `10000`+ for user policies and `30000`+ /
  `2147483647` for system ones, and they are **not** unique.

`PORT_NUMBER_RANGE` was not present on the console. `cmd/unifi` renders a
`"8000-8100"` range as `{"type":"PORT_NUMBER_RANGE","value":"8000-8100"}` by
analogy with `PORT_NUMBER`; that is still **inferred**.

#### Names are not unique; (source zone, destination zone, name) is

The console's own defaults reuse 14 names across the 67 policies — `Allow All
Traffic` ×19, `Block All Traffic` ×16, `Allow Return Traffic` ×12, `Block
Invalid Traffic` ×10. The triple **(source zone name, destination zone name,
policy name)** was verified unique across all 67, so that is the identity
`cmd/unifi` matches on. `index` is not usable as a tiebreaker: it repeats.

#### USER_DEFINED policies come back without an `id`

On 10.6.101 the API omits `id` from every `USER_DEFINED` policy — the 63
system-defined ones all carry one, the 4 user-defined ones carry none. The
ordering endpoint agrees: it returns `beforeSystemDefined: [null, null, null,
null]` for that zone pair. There is therefore **no way to address a
user-created policy** for `PUT`, `PATCH` or `DELETE` through the Integration
API on this firmware.

`cmd/unifi` can still create policies and can update the system-defined ones.
Any plan that needs to update, delete or reorder an id-less policy fails with a
message naming this limitation, rather than issuing a write it cannot target.

#### Ordering is per zone pair

`GET /firewall/policies/ordering` without arguments answers `400
api.request.error` — "Required request parameter `sourceFirewallZoneId` for
method parameter type UUID is not present". It takes
`?sourceFirewallZoneId=…&destinationFirewallZoneId=…` and returns
`{orderedFirewallPolicyIds: {beforeSystemDefined: [id], afterSystemDefined: [id]}}`
for that pair alone. A `PUT` therefore has to carry the same query parameters;
there is no site-wide ordering call.

### Clients — no DHCP reservation write path (issue #13)

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

Reservations do exist on the console; they live on the **private** controller
API (`/proxy/network/api/s/{site}/rest/user`, whose objects carry `mac`,
`name`, `use_fixedip`, `fixed_ip`, `local_dns_record`, …). `cmd/unifi` speaks
only the Integration API, so `#Client` is not modelled: a schema field the tool
could never reconcile would be worse than none. Revisit when a Network release
adds client writes to `/integration/v1`.

## Write bodies

No write has been performed against the reference console, so create/update
payloads are **inferred**: they mirror the `GET` detail representation with the
server-owned keys (`id`, `metadata`, `index`, `default`) removed. This matches
how the rest of the Integration API behaves and is the shape
`cmd/unifi/api.go` sends; the fake server in `cmd/unifi/fake_test.go` asserts
it, but a live console has not yet confirmed it. Run `unifi sync --dry-run`
first on a real deployment.

For firewall policies the inference is now checked in both directions:
`cmd/unifi` re-renders every live policy from its own projection and compares
that against the object the console returned. If they differ — because the
policy uses something `schema/unifi.cue` does not model — the policy is refused
rather than written back lossily. That check is what keeps "the write body
mirrors the read body" from being an assumption.

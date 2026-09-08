# UniFi Network Integration API — discovery notes

Field names and response shapes below were read from a live UniFi Dream Machine
Pro running **UniFi Network 10.6.101** (UniFi OS 3.0.1) on 2026-09-07. No
addresses, ids or keys are recorded here — only the schema.

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
| `/clients` | GET |
| `/info`, `/sites` | GET |

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

## Shapes (documented, NOT exercised live)

The reference console still runs the legacy firewall, so both zone-based
firewall endpoints answer `400 api.firewall.zone-based-firewall-not-configured`.
`cmd/unifi` implements them against the documented schema and treats exactly
that error code as "feature unavailable" — any other error is reported, so an
expired key can never be mistaken for an unconfigured firewall.

```
zone:   {id, name, networkIds: [string], metadata: {origin}}
policy: {id, name, description, enabled, metadata: {origin},
         action: {type: "ALLOW"|"BLOCK"|"REJECT", allowReturnTraffic},
         source|destination: {
           zoneId,
           trafficFilter: {type: "NETWORK"|"PORT",
                           networkFilter: {networkIds, matchOpposite},
                           portFilter: {type: "PORTS", matchOpposite,
                                        items: [{type: "PORT_NUMBER", port} |
                                                {type: "PORT_NUMBER_RANGE", startPort, endPort}]}}},
         ipProtocolScope: {ipVersion: "IPV4"|"IPV6"|"IPV4_AND_IPV6",
                           protocolFilter: {type: "NAMED", name}},
         connectionStateFilter: [string], loggingEnabled}
ordering (PUT /firewall/policies/ordering):
        {orderedFirewallPolicyIds: {beforeSystemDefined: [id], afterSystemDefined: [id]}}
```

## Write bodies

No write was performed against the reference console, so create/update payloads
are **inferred**: they mirror the `GET` detail representation with the
server-owned keys (`id`, `metadata`, `default`) removed. This matches how the
rest of the Integration API behaves and is the shape `cmd/unifi/api.go` sends;
the fake server in `cmd/unifi/unifi_test.go` asserts it, but a live console has
not yet confirmed it. Run `unifi sync --dry-run` first on a real deployment.

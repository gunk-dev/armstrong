# UniFi Integration API notes

`cmd/unifi/` targets the official **UniFi Network Integration API**, not the
private controller API:

- Base: `https://<console>/proxy/network/integration/v1`
- Auth: `X-API-KEY: <key>` header
- TLS: the console presents a self-signed certificate; use `UNIFI_CA_FILE` or
  `UNIFI_INSECURE_TLS=1`
- Lists page with `{"data": […], "totalCount", "offset", "limit"}`; the client
  requests `limit=200` and walks `offset` until `totalCount` is reached.

## Confirmed against a live console (Network 10.6.101)

| Endpoint | Notes |
| --- | --- |
| `GET /info` | `{"applicationVersion": "10.6.101"}` |
| `GET /sites` | `{"data":[{"id","name"}]}` — everything else hangs off `/sites/{siteId}` |
| `GET /sites/{id}/networks` | List view is an overview: no `ipv4Configuration`. The detail view (`/networks/{networkId}`) adds `ipv4Configuration`, `isolationEnabled`, `internetAccessEnabled`, `cellularBackupEnabled`, `mdnsForwardingEnabled`. The tool always fetches the detail view. |
| `GET /sites/{id}/wifi/broadcasts` | Detail view carries `securityConfiguration.passphrase` — never logged. |
| `GET /sites/{id}/dns/policies` | `{"type":"A_RECORD","domain","ipv4Address","ttlSeconds",…}` |

Observed enum values: `metadata.origin` ∈ `SYSTEM_DEFINED` | `USER_DEFINED`;
`management` = `GATEWAY`; wifi `type` = `STANDARD`, `network.type` = `NATIVE`,
`securityConfiguration.type` = `WPA2_PERSONAL`; DHCP `mode` = `SERVER`.

## Documented, not exercised live

`GET /sites/{id}/firewall/zones` and `/firewall/policies` answer
`{"message": "Zone Based Firewall is not configured"}` on a console still using
the legacy firewall. `#FirewallZone` and `#FirewallPolicy` are implemented
against the documented zone-based schema:

- Zone: `{name, networkIds[]}`
- Policy: `{name, enabled, action:{type, allowReturnTraffic}, source:{zoneId, trafficFilter}, destination:{…}, ipProtocolScope:{ipVersion, protocolFilter}, connectionStateFilter[], loggingEnabled}`
- Ordering: `PUT /firewall/policies/ordering` with
  `{"orderedFirewallPolicyIds": {"beforeSystemDefined": […], "afterSystemDefined": […]}}`

The tool treats an error from these endpoints as "feature unavailable" and
prints a `SKIP` line rather than failing, so a console on the legacy firewall
can still sync networks, wifi and DNS.

## No writes were performed during development

Schema discovery used `GET` and `OPTIONS` only. Every mutation path is
exercised against an `httptest` fake of the Integration API in `cmd/unifi/`'s
tests; `unifi sync --dry-run` was the only command pointed at the real console.

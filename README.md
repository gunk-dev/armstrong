# armstrong

Platform schema and reusable deploy workflows for the gunk-dev org. Provides shared CUE schemas for Fly.io app configuration and GitHub Actions workflows for staging, production, and preview deployments.

## CUE Schemas

Import the schemas in your CUE configuration:

```cue
import "gunk.dev/armstrong/schema"

app: schema.#FlyApp & {
    app:            "my-app-staging"
    primary_region: "ord"
    http_service: {
        internal_port: 8080
    }
}
```

Available definitions:

- `#FlyApp` — Fly.io app configuration (app name, region, HTTP service, custom domains)
- `#HttpService` — HTTP service settings (port, auto-stop, auto-start, health checks)
- `#HttpCheck` — HTTP health check configuration
- `#DNSRecord` — DNS record definition (A, AAAA, CNAME, MX, NS, SRV, TXT)
- `#Site` — a UniFi Network site (see `schema/unifi.cue`), holding `#Network`, `#FirewallZone`, `#WiFi`, `#FirewallPolicy` and `#DNSPolicy` lists

## DNS Tool

A CLI tool (`cmd/dns/`) that manages DNS records for gunk.dev via the Porkbun API.

Commands:

- `dns sync` — Reads a JSON DNS definition from stdin and converges Porkbun records to match. Use `--prune` to delete records not in the definition (skips NS, SOA, and preview-* records). Pass `--dry-run` to print the planned changes without calling the Porkbun API.
- `dns preview create <app> <pr-number>` — Creates a preview CNAME record for PR environments.
- `dns preview delete <app> <pr-number>` — Deletes a preview CNAME record.

Requires `PORKBUN_API_KEY` and `PORKBUN_SECRET_KEY` environment variables.

## Reusable Workflows

### dns-sync.yml

Syncs DNS records from a CUE definition to Porkbun. Checks out the caller repo and armstrong, builds the DNS tool, then runs the sync.

```yaml
jobs:
  dns:
    uses: gunk-dev/armstrong/.github/workflows/dns-sync.yml@main
    with:
      dry_run: false  # optional; set true to preview without mutating Porkbun
    secrets:
      PORKBUN_API_KEY: ${{ secrets.PORKBUN_API_KEY }}
      PORKBUN_SECRET_KEY: ${{ secrets.PORKBUN_SECRET_KEY }}
```

### deploy-fly.yml

Deploys an app to Fly.io for staging or production environments.

```yaml
jobs:
  deploy:
    uses: gunk-dev/armstrong/.github/workflows/deploy-fly.yml@main
    with:
      app-name: flux-staging
      cue-path: ./apps/flux
      environment: staging
      nix-target: oci-image
      image-name: flux
    secrets:
      FLY_API_TOKEN: ${{ secrets.FLY_API_TOKEN }}
```

### preview-fly.yml

Deploys preview environments from PRs and cleans them up on close.

```yaml
jobs:
  preview:
    uses: gunk-dev/armstrong/.github/workflows/preview-fly.yml@main
    with:
      app-prefix: flux-preview
      cue-path: ./apps/flux
      nix-target: oci-image
      nix-input-name: flux
      image-name: flux
      source-repo: gunk-dev/flux
      pr-number: ${{ github.event.client_payload.pr_number }}
      head-sha: ${{ github.event.client_payload.head_sha }}
      action: deploy
    secrets:
      FLY_API_TOKEN: ${{ secrets.FLY_API_TOKEN }}
      APP_ID: ${{ secrets.APP_ID }}
      APP_PRIVATE_KEY: ${{ secrets.APP_PRIVATE_KEY }}
```

## UniFi Tool

A CLI tool (`cmd/unifi/`) that manages a UniFi Network site via the official
**Integration API** served by the console at
`https://<console>/proxy/network/integration/v1`. It is the sibling of the DNS
tool: it reads a JSON document on stdin (pipe from `cue export`) and converges
the console to match.

Commands:

- `unifi export` — Dumps the live site as `#Site`-shaped JSON so a consumer repo can bootstrap its instance file from real state. WiFi passphrases are never included; each SSID gets a `passphraseEnv` name instead.
- `unifi diff` — Reads `#Site` JSON from stdin and prints the plan without changing anything. Exits non-zero when a change would be made, for CI. Pass `--prune` to include deletions in the plan.
- `unifi sync [--prune] [--dry-run]` — Reads `#Site` JSON from stdin and converges the site. `--prune` deletes `USER_DEFINED` objects absent from the input; `--dry-run` prints the plan without calling the API.

Environment:

| Variable | Meaning |
| --- | --- |
| `UNIFI_URL` | Console base URL, e.g. `https://192.168.1.1` |
| `UNIFI_API_KEY` | Integration API key (Settings → Control Plane → Integrations). Never printed. |
| `UNIFI_SITE` | Site name, default `Default` |
| `UNIFI_CA_FILE` | PEM bundle for the console's self-signed certificate |
| `UNIFI_INSECURE_TLS` | Set to `1` to skip certificate verification instead |
| `UNIFI_WIFI_*` | Whatever `passphraseEnv` names your `#WiFi` entries reference |

### Rules

- **Names are the identity.** Ids are server-assigned and must never appear in a
  consumer repo, so desired and actual objects are matched by `name` (DNS
  policies, which have no name, are matched by type plus domain).
- **`SYSTEM_DEFINED` objects are never deleted**, not even with `--prune`. Their
  configurable fields are updated when the instance file declares them, so you
  can manage the console's own default LAN.
- **Secrets stay out of git.** `#WiFiSecurity` carries `passphraseEnv` — the name
  of an environment variable — not the passphrase. Passphrases and the API key
  are redacted from all output.
- Resources are reconciled in dependency order: networks → firewall zones →
  wifi, firewall policies, DNS policies.

### Running it

`unifi sync` is meant to run on a **LAN host** (e.g. under a NixOS systemd
timer), not in GitHub Actions: the console is only reachable on the local
network and presents a self-signed certificate.

```sh
cue export ./unifi --out json -e site | unifi sync --prune
```

See `examples/unifi/site.cue` for a complete example instance (RFC 5737
documentation addresses) and `docs/unifi.md` for the API details, including
which parts are confirmed against a live console and which are documented but
untested.

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

## DNS Tool

A CLI tool (`cmd/dns/`) that manages DNS records for gunk.dev via the Porkbun API.

Commands:

- `dns sync` — Reads a JSON DNS definition from stdin and converges Porkbun records to match. Use `--prune` to delete records not in the definition (skips NS, SOA, and preview-* records).
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

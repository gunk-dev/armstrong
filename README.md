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
- `#DNSRecord` — DNS record definition (A, CNAME, TXT, MX, AAAA)

## Reusable Workflows

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

# Repository context

## Purpose
`armstrong` is a platform repo for the `gunk-dev` GitHub org. It ships two things consumed by other repos: (1) shared CUE schemas (`schema/`) that other repos import to describe Fly.io app config and DNS records, and (2) reusable GitHub Actions workflows (`.github/workflows/*.yml` with `on: workflow_call`) that other repos call to deploy to Fly.io and to sync DNS records against Porkbun. It also contains a single Go CLI (`cmd/dns/`) used by the DNS sync workflow.

## Tech stack
- Go 1.25.0 (`go.mod`), module path `gunk.dev/armstrong`.
- CUE v0.9.2 — module declared in `cue.mod/module.cue`; CI installs the same version (`.github/workflows/ci.yml`).
- Cobra v1.10.2 for the CLI (`github.com/spf13/cobra` in `go.mod`; used in `cmd/dns/main.go`, `cmd/dns/sync.go`, `cmd/dns/preview.go`). No other direct Go dependencies — the standard library handles HTTP/JSON for the Porkbun client.
- GitHub Actions for CI and reusable workflows. Workflows pin actions by SHA.
- Nix is used by the reusable Fly.io deploy workflows (`nix develop -c …`, `nix build …`) — there is no flake in this repo; the caller repo is expected to provide it.

## Entry points
- `cmd/dns/main.go` — `dns` CLI binary. Registers `sync` and `preview` subcommands on a Cobra root command.
- `schema/fly.cue` — CUE package `schema` exposing `#FlyApp`, `#HttpService`, `#HttpCheck`.
- `schema/dns.cue` — CUE package `schema` exposing `#DNSRecord`.
- `.github/workflows/dns-sync.yml` — reusable workflow that builds the DNS tool and runs `dns sync --prune` against caller-provided CUE.
- `.github/workflows/deploy-fly.yml` — reusable workflow for staging/prod Fly.io deploys.
- `.github/workflows/preview-fly.yml` — reusable workflow for PR preview deploy and cleanup.

## Layout
- `cmd/dns/` — Go source for the `dns` CLI. `main.go` (root command), `sync.go` (`dns sync`), `preview.go` (`dns preview create|delete`), `porkbun.go` (Porkbun API client), `sync_test.go` (unit tests).
- `schema/` — CUE definitions imported by other repos as `gunk.dev/armstrong/schema`.
- `cue.mod/` — CUE module metadata (`module.cue` only; no `pkg/` vendoring).
- `.github/workflows/` — `ci.yml` for this repo's CI, plus three `workflow_call` reusable workflows (`dns-sync.yml`, `deploy-fly.yml`, `preview-fly.yml`).
- `.claude/` — local Claude Code settings (`settings.json`).

## Build, test, run
- Build everything: `go build ./...` (see CI step in `.github/workflows/ci.yml`).
- Run Go tests: `go test ./...` (CI uses the same command).
- Build the DNS CLI specifically: `go build -o dns-tool ./cmd/dns/` (matches the `.github/workflows/dns-sync.yml` workflow step).
- Validate CUE: `cue vet ./schema/` (CI step). Requires `cue` v0.9.2 on PATH.
- Lint workflow YAML locally: `yamllint -d "{extends: default, rules: {line-length: disable, truthy: disable}}" .github/workflows/` (CI step).
- Static-analyse workflows: `zizmor . --no-online-audits` (CI step).
- Run `dns sync` locally: `cue export ./dns --out json | dns-tool sync [--prune] [--dry-run]`. Requires `PORKBUN_API_KEY` and `PORKBUN_SECRET_KEY` env vars (`cmd/dns/porkbun.go` errors out otherwise). The CUE path (`./dns`) is supplied by the caller repo, not by this repo.

## Conventions
- Shared CUE goes in `schema/` under package `schema` (`schema/fly.cue:1`, `schema/dns.cue:1`); importers reference it as `gunk.dev/armstrong/schema`.
- The `dns sync` command never prunes `NS`, `SOA`, or any record whose subdomain starts with `preview-` even when `--prune` is passed (`cmd/dns/sync.go:151-163`). Preserve this safety net when modifying prune logic.
- Porkbun mutations go through the `porkbunMutator` interface (`cmd/dns/sync.go:30-35`) so tests can inject a fake and assert no API calls occur under `--dry-run`.
- AAAA record content is normalised via `net.ParseIP` before comparison so equivalent IPv6 textual forms don't cause spurious updates (`cmd/dns/sync.go:202-208`).
- Reusable workflows declare `permissions: {}` at the top level and grant only what each job needs; `actions/checkout` is always invoked with `persist-credentials: false` (see all four workflow files).
- GitHub Actions are pinned by full commit SHA, not tag, with the version in a trailing comment (e.g. `actions/checkout@b4ffde65...` `# v4`).
- User-supplied workflow inputs are passed to shell steps via `env:` rather than interpolated directly into `run:` scripts, to avoid shell injection (e.g. `.github/workflows/deploy-fly.yml`, `.github/workflows/preview-fly.yml` validation step).
- Errors are wrapped with `%w` and context (`cmd/dns/sync.go`, `cmd/dns/porkbun.go`).

## Gotchas
- The `.github/workflows/dns-sync.yml` reusable workflow checks out the caller repo at the workspace root **and** checks out `gunk-dev/armstrong` into `_armstrong/`, then runs `cue export ./dns` against the caller and pipes into `./_armstrong/dns-tool sync`. The caller must therefore have a top-level `./dns` CUE package; this is not configurable via input.
- `dns sync` reads JSON from stdin only — there is no file-path flag (`cmd/dns/sync.go:56`).
- The `preview` subcommand hardcodes `defaultDomain = "gunk.dev"` (`cmd/dns/preview.go:10`) and the CNAME target shape `<app>-preview-<pr>.fly.dev` (`cmd/dns/preview.go:43`). Changing either requires a code change, not config.
- `.github/workflows/deploy-fly.yml` and `.github/workflows/preview-fly.yml` rely on the caller's Nix flake providing the requested `nix-target` and (for previews) a flake input named `nix-input-name` that can be overridden via `--override-input`. There is no flake in this repo to test against.
- The smoke test in `.github/workflows/deploy-fly.yml` curls `http://localhost:8080` against the just-loaded image (`.github/workflows/deploy-fly.yml:96-104`) — apps that don't listen on 8080 will fail the smoke test even if otherwise healthy.

## External dependencies
- Porkbun DNS API (`https://api.porkbun.com/api/json/v3`, `cmd/dns/porkbun.go:12`) — requires `PORKBUN_API_KEY` and `PORKBUN_SECRET_KEY`.
- Fly.io — reusable deploy workflows shell out to the `fly` CLI (provided by the caller's Nix devshell) and push images to `registry.fly.io`. Require `FLY_API_TOKEN`.
- GitHub App credentials (`APP_ID`, `APP_PRIVATE_KEY`) used by `.github/workflows/preview-fly.yml` to mint a token via `actions/create-github-app-token` so it can comment the preview URL back on the source-repo PR.

# armstrong

Platform schema and reusable deploy workflows for the gunk-dev org. Provides shared CUE schemas for Fly.io app configuration and GitHub Actions workflows for staging, production, and preview deployments.

## Consuming from Nix

This repo is a flake, so a consumer gets the CLIs and the CUE schemas from one
pinned input — no `flake = false`, no hand-maintained `vendorHash`, and no
`.cue` files vendored into the consumer's git tree.

```nix
{
  inputs.armstrong.url = "github:gunk-dev/armstrong";
  inputs.armstrong.inputs.nixpkgs.follows = "nixpkgs";
}
```

Outputs, for `x86_64-linux` and `aarch64-linux`:

| Output | Contents |
| --- | --- |
| `packages.<system>.unifi` | the `unifi` CLI (also `packages.<system>.default`) |
| `packages.<system>.dns` | the `dns` CLI |
| `packages.<system>.schema` | `schema/*.cue` laid out as a CUE module package path |
| `devShells.<system>.default` | go, cue, nixfmt, gh |
| `nixosModules.unifi-sync` | the NixOS unit that keeps a UniFi site converged (also `nixosModules.default`) |
| `checks.<system>` | `go vet`, `go test ./...`, `cue vet ./schema`, `nixfmt --check`, a build of `examples/unifi` against `packages.schema`, and the `unifi-sync` module's VM test |

### The CLIs

Run one without installing anything:

```sh
nix run github:gunk-dev/armstrong#unifi -- --help
```

Or reference the package directly:

```nix
{ pkgs, inputs, ... }:
{
  environment.systemPackages = [
    inputs.armstrong.packages.${pkgs.stdenv.hostPlatform.system}.unifi
  ];
}
```

To actually run `unifi` on a schedule, import
[`nixosModules.unifi-sync`](#nixos-module) rather than writing the unit by
hand.

### The schema

`packages.<system>.schema` is a derivation whose output is already shaped like a
CUE module's package directory:

```
$out/cue.mod/pkg/gunk.dev/armstrong/VERSION       # the armstrong git rev, for provenance
$out/cue.mod/pkg/gunk.dev/armstrong/schema/*.cue
```

So a consumer assembles its `cue.mod/pkg` at build time and keeps it out of git.
`cue.mod/module.cue` stays in the consumer repo; only the vendored package tree
comes from the flake:

```nix
{ pkgs, inputs, ... }:
let
  schema = inputs.armstrong.packages.${pkgs.stdenv.hostPlatform.system}.schema;
in
# ./. holds the consumer's cue.mod/module.cue and its ./unifi CUE package.
pkgs.runCommand "unifi-site.json" { nativeBuildInputs = [ pkgs.cue ]; } ''
  cp -R ${./.} src && chmod -R u+w src && cd src
  mkdir -p cue.mod/pkg/gunk.dev
  ln -s ${schema}/cue.mod/pkg/gunk.dev/armstrong cue.mod/pkg/gunk.dev/armstrong
  export CUE_CACHE_DIR=$TMPDIR/cue
  cue export ./unifi --out json -e site > $out
''
```

The consumer's `cue.mod/module.cue` must declare a `language: version` — `cue`
refuses to resolve `cue.mod/pkg` without one.

The consumer's CUE then imports the schemas by module path as usual:

```cue
import "gunk.dev/armstrong/schema"

site: schema.#Site & { /* ... */ }
```

Bumping the schemas is `nix flake update armstrong` — the CLIs and the CUE
definitions move together, and `VERSION` records which rev is in use.

## NixOS module

`nixosModules.unifi-sync` is the actuator for `#Site`: a hardened oneshot that
runs the same pipeline a human runs by hand —

```sh
cue export ./instance --out json -e site | unifi <diff|sync> [--prune]
```

— plus a daily drift timer and a `unifi-plan` wrapper on `PATH`. It is meant
for a **LAN host**: the console is only reachable on the local network.

`instance` is a path inside *your* flake, so it is copied to the store at build
time and the unit runs against the exact tree the host converged to — there is
no second copy of the desired state on the machine to drift from the
repository. The schema the instance is validated against and the binary that
consumes the export both come from the pinned `armstrong` input, so they cannot
disagree.

### Minimal host

```nix
{ config, inputs, ... }:
{
  imports = [ inputs.armstrong.nixosModules.unifi-sync ];

  modules.unifi-sync = {
    enable = true;
    consoleUrl = "https://198.51.100.1";
    insecureTls = true;              # or caFile, see below
    instance = ./net/unifi;          # holds `site: schema.#Site & {…}`
    apiKeyFile = config.age.secrets."unifi-api-key".path;
    secretsFile = config.age.secrets."unifi-wifi.env".path;

    # Reconcile as soon as the host finishes converging, not just daily.
    onSuccessOf = [ "nixos-upgrade" ];
  };
}
```

That host is in **diff mode**: it never writes. `unifi diff` exits 2 when the
plan is non-empty, so drift leaves `unifi-sync.service` failed with the plan
itself in the journal:

```console
$ systemctl status unifi-sync
   Active: failed (Result: exit-code)
$ journalctl -u unifi-sync -n 20
UPDATE network        Default (mdnsForwardingEnabled)
UPDATE dns policy     A_RECORD host.example.invalid
```

Turning the detector into an actuator is one line — `mode = "sync"` — and is
deliberately a separate decision from adopting the module. Whatever `mode` is,
`unifi-plan` prints the plan without changing anything: it is always `sync
--dry-run`.

### Options

All under `modules.unifi-sync`:

| Option | Default | Meaning |
| --- | --- | --- |
| `enable` | `false` | Install the unit and the timer. |
| `consoleUrl` | — | Base URL of the console; the Integration API lives under `/proxy/network/integration/v1` on it. |
| `site` | `"Default"` | Site name, as shown in the console's site list. |
| `instance` | — | CUE package directory holding `site: schema.#Site & {…}`. |
| `mode` | `"diff"` | `diff` never writes and fails on drift; `sync` applies the plan. |
| `prune` | `false` | Delete `USER_DEFINED` objects the instance file does not declare. |
| `apiKeyFile` | — | File holding the Integration API key. Reaches the unit as a systemd credential. |
| `secretsFile` | `null` | `EnvironmentFile` of SSID passphrases, one `NAME=value` per `passphraseEnv`. |
| `caFile` | `null` | PEM bundle pinning the console's certificate (`UNIFI_CA_FILE`). |
| `insecureTls` | `false` | Skip certificate verification instead (`UNIFI_INSECURE_TLS=1`). |
| `onSuccessOf` | `[ ]` | Unit names (no `.service`) whose success triggers a run. |
| `after` | `[ "network-online.target" ]` | `After=` for the unit; the default target also gets a matching `Wants=`. |
| `wantedBy` | `[ ]` | `WantedBy=` for the unit. |
| `checkAt` | `"05:30"` | `OnCalendar` for the daily drift check, or `null` for no timer. |
| `package` | `packages.<system>.unifi` | The `unifi` build to run. |
| `schema` | `packages.<system>.schema` | Where `gunk.dev/armstrong/schema` is resolved from. |
| `moduleFile` | armstrong's own | The `cue.mod/module.cue` the store module is assembled around. |

`package` and `schema` default to this flake's outputs, so the module works
with nothing but an import; override them to run a different build of either.
`moduleFile` exists because `cue` finds a module by walking up from the working
directory, so the CUE module has to be reassembled in the store around
`instance` rather than pointed at — set it to your repository's own
`cue.mod/module.cue` if you want the instance evaluated under exactly the
module identity and language version you use elsewhere.

A console reached over `https` serves a certificate no public CA signed, so
exactly one of `caFile` or `insecureTls` has to be set: an assertion fails the
build otherwise, because silently pinning nothing is the failure mode a TLS
option must never have.

### Secrets

Two files, both outside the store, both typically agenix or sops-nix paths:

- `apiKeyFile` — the Integration API key (Settings → Control Plane →
  Integrations). It is passed as a systemd `LoadCredential`, never through the
  unit's environment where anything able to read `/proc` could lift it.
- `secretsFile` — an `EnvironmentFile` with one line per SSID passphrase, named
  by the `passphraseEnv` of the matching `#WiFi`:

  ```sh
  # SSID passphrases, by passphraseEnv.
  UNIFI_PSK_HOME=correct-horse-battery
  ```

  Required whenever the instance declares a non-OPEN SSID — `unifi` fails
  rather than write an empty passphrase.

`unifi-plan`, run by hand, reads both files directly, so it needs enough
privilege to; run under `sudo` or it will report every SSID as needing a
passphrase update (and say so on stderr).

The unit runs `DynamicUser`, `ProtectSystem=strict`, with an empty capability
bounding set and no address families beyond `AF_UNIX`/`AF_INET`/`AF_INET6`: it
talks to one endpoint and writes nothing to disk.

### Testing it

`checks.<system>.unifi-sync-vm` boots a host running the real unit against a
fake console and asserts that a clean site succeeds, a drifted site leaves the
unit failed with the plan in the journal, and that neither run issues a single
write. It needs KVM, which GitHub-hosted `ubuntu-latest` runners have;
`nix flake check` on a machine without `/dev/kvm` will not be able to build it.
`checks.<system>.unifi-sync-eval` imports the module into a scratch
`nixosSystem` and needs no VM.

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
- `unifi diff` — Reads `#Site` JSON from stdin and prints the plan without changing anything. Exits 2 when a change would be made and 1 on failure, so CI can tell drift apart from a broken run. Pass `--prune` to include deletions in the plan.
- `unifi sync [--prune] [--dry-run]` — Reads `#Site` JSON from stdin and converges the site. `--prune` deletes `USER_DEFINED` objects absent from the input; `--dry-run` prints the plan without calling the API.

Environment:

| Variable | Meaning |
| --- | --- |
| `UNIFI_URL` | Console base URL, e.g. `https://unifi.lan` |
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
- **`--prune` only acts on resource types the instance file declares.** An empty
  `wifi` list deletes no SSIDs — an instance file that simply forgot a section
  must not wipe it.
- **Secrets stay out of git.** `#WiFiSecurity` carries `passphraseEnv` — the name
  of an environment variable — not the passphrase. Passphrases and the API key
  are redacted from all output, including API error responses.
- Resources are reconciled in dependency order: networks → firewall zones →
  wifi, firewall policies, DNS policies.

### Running it

`unifi sync` is meant to run on a **LAN host**, not in GitHub Actions: the
console is only reachable on the local network and presents a self-signed
certificate.

```sh
cue export ./unifi --out json -e site | unifi sync --prune
```

On NixOS, `nixosModules.unifi-sync` runs exactly that pipeline as a hardened
oneshot on a timer — see [NixOS module](#nixos-module).

See `examples/unifi/site.cue` for a complete example instance (RFC 5737
documentation addresses), `docs/unifi.md` for the full guide, and
`docs/unifi-api-notes.md` for the API shapes — including which are confirmed
against a live console and which are documented but not yet exercised.

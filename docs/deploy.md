# Deploying from an infra revision with `cmd/deploy`

`deploy` works from an exact infra commit. Use `app <name>` to build and deploy one Fly app or
`dns-sync` to sync the repository's Porkbun zones. Use `converge` to sync DNS and deploy the flake's
apps from the same commit, skipping apps that meet the skip rule below. Each attempt runs under a
lock and writes a record. The command reports its outcome through its exit status.

The infra repository owns identities, rendering, builds and checks; `deploy` owns the layout it
reads from that repository and the steps it runs.

## Invocation

```sh
export DEPLOY_STATE_DIR=/var/lib/deploy

deploy app <name> --infra <checkout|git-url> --rev <commit> --fly-token-file <file>
deploy dns-sync   --infra <checkout|git-url> --rev <commit> \
  --porkbun-api-key-file <file> --porkbun-secret-key-file <file> [--dry-run]
deploy converge   --infra <checkout|git-url> (--rev <commit> | --commit-file <file>) \
  --fly-token-file <file> \
  --porkbun-api-key-file <file> --porkbun-secret-key-file <file> [--dry-run]
```

| Input                                                 | Meaning                                                                                                                                                                                                                                                                                                                                                                   |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--infra`                                             | A repository directory, or an `https://` or `file://` Git URL. URLs carry no credentials, query or fragment; SSH and plain HTTP are refused                                                                                                                                                                                                                               |
| `--rev`                                               | The full 40-character lowercase commit (mutually exclusive with `--commit-file` on `converge`)                                                                                                                                                                                                                                                                            |
| `--commit-file`                                       | `converge` only, mutually exclusive with `--rev`: a file of `NAME=value` lines read once as data. Takes `VALLEY_COMMIT` (40-character lowercase commit), ignores other names, refuses a missing, duplicated or malformed value with exit 2 before anything starts, and requires the commit to be an ancestor of (or equal to) `refs/heads/main` before any attempt starts |
| `DEPLOY_STATE_DIR`                                    | An absolute directory holding `deploy.lock`, `apps/` and `runs/`, created if missing. No path component may be a symlink. It must belong to the running user and be writable by no one else; every ancestor must belong to that user or root (or the kernel's overflow uid for `/` inside a user namespace) and be writable by no one else unless sticky, like `/tmp`     |
| `--fly-token-file`                                    | `app` and `converge`: the Fly API token                                                                                                                                                                                                                                                                                                                                   |
| `--porkbun-api-key-file`, `--porkbun-secret-key-file` | `dns-sync` and `converge`: the Porkbun key pair                                                                                                                                                                                                                                                                                                                           |
| `--dry-run`                                           | `dns-sync` and `converge`: print the plan or per-app decisions, change nothing                                                                                                                                                                                                                                                                                            |

Credential files must be non-empty; terminal newlines are trimmed. Extra arguments and any other
flag, including target or environment overrides such as `--app`, are usage errors. `app` has no dry
run. `--help` and `--version` work as usual; the version is stamped at packaging time.

## Exit status

| Status        | Meaning                                             |
| ------------- | --------------------------------------------------- |
| 0             | completed, skipped, or a completed dry run          |
| 2             | usage error; nothing was started                    |
| 75            | another attempt holds the lock; nothing was started |
| 129, 130, 143 | interrupted by SIGHUP, SIGINT or SIGTERM            |
| 1             | anything else, including partial completion         |

## Infra layout

For `app <name>`, the infra flake provides, for the Nix system `deploy` was packaged for:

| Output                                   | Contents                                                                                                                                                                                                                                           |
| ---------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `packages.<system>.deploy-<name>-image`  | One single-platform OCI archive whose derivation depends on that image's smoke check. The tar must include `blobs/` and `blobs/sha256/` directory entries (as skopeo's tars do), because unprivileged skopeo cannot unpack an archive without them |
| `packages.<system>.deploy-<name>-config` | A directory holding only `fly.json`; its `app` names the Fly app, and nothing else selects the target                                                                                                                                              |

Each output, and the config's `fly.json`, must resolve to a path inside the Nix store: an output
that links to a host path could change after it was checked. The image's platform must match the
packaged system: `linux/amd64` for `x86_64-linux`, `linux/arm64` for `aarch64-linux`. The app, its
Machines, and any certificates must already exist; `deploy` provisions nothing.

For `dns-sync`, the zones live under `dns/`. Each immediate directory of `dns/` is one CUE zone
package, taken in sorted order and addressed as `./dns/<zone>/`; with no directories, `dns/` itself
is the one zone. Every zone exports a `records` list (`records: []` for an empty zone):
`dns sync --prune` would read a zone without one as empty and delete its records. A zone directory
is committed as a directory, not as a link to one, and no committed symlink may point outside the
commit, so neither zones nor the packages they import come from anywhere else.

## Source

`deploy` fetches the requested commit into a fresh private repository, checks that the resolved
commit is exact, and checks it out detached. With `--commit-file`, `deploy` fetches
`refs/heads/main` from `--infra` and requires the commit to be an ancestor of (or equal to)
`refs/heads/main`. Working-tree changes in a checkout never reach the build. The commit is refused
if it contains a gitlink or has no committed `flake.lock`. Git runs with no configuration files, no
credential helpers, no prompts, and only the file and HTTPS transports.

Nix builds the checkout as `git+file://<checkout>?rev=<commit>` with `--no-update-lock-file`,
`--no-write-lock-file`, `accept-flake-config` off, and the `nix-command` and `flakes` features
enabled. `flake.lock` must remain unchanged afterwards. No input overrides or development shells are
used; every tool comes from the pinned tool path.

## Phases

Every attempt records the phases `source`, `prepare`, `inspect`, `upload`, `apply`, `verify` and
`cleanup`, in that order.

| Phase     | `app`                                                                                                                                                                 | `dns-sync`                                                             |
| --------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `source`  | fetch and check the commit                                                                                                                                            | the same                                                               |
| `prepare` | build the config, then the image, and validate both                                                                                                                   | vet every zone, then export them all into `zones.json`                 |
| `inspect` | record every Machine and volume; refuse an app with no Machines                                                                                                       | dry run: `dns sync --prune --dry-run`, plan printed. Otherwise skipped |
| `upload`  | push to `registry.fly.io/<app>:<attempt-id>` with `skopeo copy --preserve-digests`, then compare the SHA-256 of the registry's manifest with the local image manifest | skipped                                                                |
| `apply`   | `fly deploy --config <fly.json> --app <app> --image registry.fly.io/<app>@<digest>`, then record the app again                                                        | `dns sync --prune`. Skipped in a dry run                               |
| `verify`  | require the same volume IDs and Machine count as before, every Machine on the deployed digest, and a started Machine unless `fly.json` lets every Machine stop        | skipped: `dns sync`'s exit status decides                              |
| `cleanup` | remove the checkout, build links, credentials and temporary files                                                                                                     | the same                                                               |

`fly deploy` runs an ordinary deploy using its health checks, the rendered mounts, and no custom
Machine update strategy. Replacing a Machine is allowed. A changed set of volume IDs fails
verification at once. Otherwise verification judges each snapshot, the first taken as `fly deploy`
returns, and polls a fresh one every two seconds for up to 60 seconds until the app passes, because
flyctl offers no event for a Machine changing image.

Verification reads each Machine's `state` and `image_ref.digest` from `flyctl machines list --json`,
and from the `fly.json` bytes that were hashed the keys `http_service` and `services`, with their
`auto_stop_machines`, `min_machines_running` and `processes`, and the top-level `processes`. It
depends on those keys being spelled as this repository's Fly schema emits them (`schema/fly.cue`).
The app passes when:

- no Machine is in a state other than `started`, `stopped` or `suspended`, which is pending;
- the Machine count is as before: a Machine being replaced can show briefly beside its replacement,
  so a different count is waited on too;
- every Machine reports the deployed digest, since a stopped or suspended Machine on the old image
  would start or resume it; and
- at least one Machine is `started`, if `fly.json` keeps one running: an `http_service` or
  `services` entry with `min_machines_running` above zero, or with `auto_stop_machines` absent or
  off, or no service at all. When every service sets `auto_stop_machines` to `"stop"` or `"suspend"`
  with `min_machines_running` zero or absent, an app whose Machines are all stopped or suspended
  passes once each reports the deployed digest. A `fly.json` with a `processes` section, or with any
  service that has a `processes` key, always requires a started Machine.

A snapshot starts only with at least five poll intervals of the window left. When the window ends
without a pass, `verify` is `uncertain` if a Machine is still pending or the most recent snapshot
failed, whether it errored or hung until the window closed, since the outcome is then unknown, and
`failed` otherwise. An app that cannot be observed at all after the deploy is `uncertain` too.
`targets_after` records the last snapshot.

One thing this rests on has not been observed live: that Fly reports the new image in `image_ref`
for a Machine `fly deploy` updated while it was stopped or suspended, without starting it.

A phase ends `completed` (verified success), `failed` (a known failure), `skipped` (with a reason),
or `uncertain`. A phase is `uncertain` if and only if a change to the deployment target started in
it and was neither confirmed complete nor confirmed not to have happened. Only `fly deploy` and the
real `dns sync` change the target: either one interrupted, timed out or exiting nonzero leaves
`apply` uncertain, except a `dns sync` exit status of 3, which it gives when it failed before
attempting any write. Verification after a completed apply is uncertain when it could not observe
the app, a Machine had not settled, or it never ran because of an interruption or a recording
failure. Every other failure, interrupted or not, is `failed`: a git, nix or cue step, reading the
app or DNS records, and pushing the attempt's unreferenced image tag.

The first failed or uncertain phase stops the attempt; the remaining phases are skipped, and
`cleanup` always runs. The last line `deploy` prints says `completed`, `failed` or `uncertain`.
Nothing is recovered automatically:

- after an uncertain app deploy, look at the app's Machines (`fly machines list`) before running
  again;
- after an uncertain DNS sync, run `deploy dns-sync --dry-run`, which shows what differs. The sync
  is safe to run again once the cause is understood.

## Record

The attempt creates `runs/<attempt-id>/record.json` once it holds the lock and prints the record's
absolute path first. The record is rewritten atomically and synced to disk before and after every
phase. A phase is written as `uncertain` before any of its children start, so a crash leaves it
uncertain. Directories are 0700 and files 0600. Records and their logs are kept indefinitely;
everything else the attempt creates is removed by `cleanup`.

| Field                             | Meaning                                                                                                                                                                               |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `schema_version`                  | `1`                                                                                                                                                                                   |
| `attempt_id`                      | the run directory's random name, also the image tag                                                                                                                                   |
| `verb`                            | `app` or `dns-sync`                                                                                                                                                                   |
| `name`                            | the `app` command's `<name>`, set when the record is created; null for `dns-sync`                                                                                                     |
| `dry_run`                         | whether `--dry-run` was given                                                                                                                                                         |
| `source`                          | `--infra`, as an absolute directory or the URL                                                                                                                                        |
| `requested_commit`                | the commit from `--rev` or `--commit-file`                                                                                                                                            |
| `resolved_commit`                 | the commit verified by `source`                                                                                                                                                       |
| `lock_sha256`                     | SHA-256 of the committed `flake.lock`                                                                                                                                                 |
| `config_sha256`                   | SHA-256 of each config file by name: `fly.json`, or `zones.json`                                                                                                                      |
| `platform`                        | the packaged Nix system                                                                                                                                                               |
| `runner_version`                  | `deploy --version`                                                                                                                                                                    |
| `image_digest`                    | the image manifest's `sha256:` digest                                                                                                                                                 |
| `targets_before`, `targets_after` | the app as inspect observed it, and as last observed after apply; null for `dns-sync`                                                                                                 |
| `started_at`, `finished_at`       | UTC RFC 3339 timestamps with nanoseconds                                                                                                                                              |
| `outcome`                         | `uncertain` if any phase is, else `failed` if any phase is, else `skipped` when `converge` skipped an already-converged app, else `completed`; `uncertain` until the attempt finishes |
| `phases`                          | `name`, `outcome` and `reason` per phase                                                                                                                                              |
| `logs`                            | the child logs' directory, relative to the run: `logs`                                                                                                                                |

Unavailable values are null. Targets have this shape, sorted by ID:

```json
{
  "app": "demo-app",
  "machines": [{"id": "m1", "state": "started", "image_digest": "sha256:..."}],
  "volumes": [{"id": "vol1", "attached_machine_id": "m1"}]
}
```

If the record cannot be written, no new work starts. Running children are still terminated and
reaped, `cleanup` runs, and the final record is attempted. The next phase — or `cleanup` when no
phase remains — records the write error as its own failure, so a final record that lands explains
the nonzero exit.

## Converge

`deploy converge` runs a sequence of ordinary attempts under one lock on
`DEPLOY_STATE_DIR/deploy.lock`: `dns-sync` first, then each app the infra flake exposes for the
packaged Nix system, in sorted order. Each attempt writes its own `runs/<attempt-id>/record.json`
and `logs/` and follows the same credential, redaction, signal and cleanup rules as a standalone
invocation.

### App discovery

Before syncing DNS, `converge` checks the commit and `flake.lock` using the `source` rules
(including the `refs/heads/main` ancestry check when `--commit-file` is used). It lists
`packages.<system>` without building images, checks that every `deploy-<name>-image` and
`deploy-<name>-config` is paired with a name matching `^[a-z0-9][a-z0-9-]*$`, and builds each app's
`deploy-<name>-config` to validate `fly.json` and resolve its Fly app name. A missing partner, an
invalid name or config, or two outputs targeting the same Fly app stops the run before any attempt
record is written. With no app pairs and no discovery errors, only DNS runs.

### Skipping an app

Fly adds defaults and translates `fly.json` into Machine settings, so deploy checks the last
recorded config against the requested config. When either app command reaches `apply` outside a dry
run, it atomically updates `DEPLOY_STATE_DIR/apps/<fly-app>` under the state lock to point to that
attempt's record, even if the attempt fails or ends uncertain.

Under `converge`, after `source`, `prepare` and `inspect` succeed for an app, the app is skipped if
and only if all of the following hold:

1. `DEPLOY_STATE_DIR/apps/<fly-app>` is a readable regular file (not a symlink) inside a private
   `apps/` directory, naming a readable, valid schema-version-1 non-dry-run `app` record at
   `runs/<id>/record.json` whose `attempt_id` matches the pointer.
2. That record ended `completed`, with the same flake `name`, `platform`, Fly app
   (`targets_after.app`), `config_sha256["fly.json"]`, and `image_digest` as the current build.
3. The live snapshot observed by `inspect` (`targets_before`) shows every Machine in a settled state
   (`started`, `stopped` or `suspended`) reporting that `image_digest`, and at least one `started`
   Machine whenever `fly.json` keeps one running.

If any check fails, `converge` deploys the app.

A skip confirms the live image and started-Machine requirement against the requested build. The
config check uses the last config deploy applied, so manual changes to live settings can go
undetected. Run `deploy app <name>` to deploy unconditionally.

When an app is skipped, `upload`, `apply` and `verify` are recorded as `skipped` with a reason
stating what was compared — the live Machines' image digest and the last deploy's `fly.json` hash,
naming the matching attempt ID — and `converge` prints that same reason in its summary. `cleanup`
runs, and the attempt ends with `outcome` set to `skipped`.

### Stopping, summary and dry run

Convergence stops without retrying if an attempt fails, ends uncertain, or cannot write its record.
It also stops on a handled signal. See [Exit status](#exit-status) for the exit codes. The
`summary:` on stdout lists targets in order with outcomes, available record paths, and reasons for
skips and dry-run decisions. It marks remaining targets `not attempted`.

A dry run prints the redacted DNS plan and explains which apps it would skip or deploy. It still
builds outputs, reads live Machine state and writes records, while leaving DNS, Fly apps and the
registry unchanged. Its records cannot justify a later skip.

### Repetition safety

You can rerun convergence at the same commit after a restart, or jump directly to a later commit.
DNS converges to the requested zones each time. A skipped app still reads live state from Fly and
makes no changes to the app or registry. Check the target before rerunning an uncertain attempt.

## NixOS module

`nixosModules.deploy` (options under `modules.deploy`) wires a host to run `deploy converge` from an
external trigger.

| Option                 | Default                               | Meaning                                                                                                                      |
| ---------------------- | ------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `enable`               | `false`                               | Enable `deploy-converge.service` and `deploy-snapshot-refresh.service`                                                       |
| `package`              | `armstrong.packages.${system}.deploy` | The `deploy` package to run                                                                                                  |
| `user`                 | `"deploy"`                            | System user `deploy-converge.service` runs as and owns `stateDir` under; created automatically when left at `"deploy"`       |
| `group`                | `"deploy"`                            | Primary group for `user`; created automatically when left at `"deploy"`                                                      |
| `stateDir`             | `"/var/lib/deploy"`                   | Absolute `DEPLOY_STATE_DIR` holding `deploy.lock`, `apps/`, `runs/` and `snapshot.git` (mode 0700)                           |
| `sourceRepository`     | _(required)_                          | Absolute path to the infra Git repository on disk whose `refs/heads/main` is snapshotted; need not be readable by `user`     |
| `commitFile`           | _(required)_                          | Trigger file of `NAME=value` lines holding `VALLEY_COMMIT`, readable by `user` and passed to `deploy converge --commit-file` |
| `flyTokenFile`         | _(required)_                          | File holding the Fly API token, loaded through systemd `LoadCredential`                                                      |
| `porkbunApiKeyFile`    | _(required)_                          | File holding the Porkbun API key, loaded through systemd `LoadCredential`                                                    |
| `porkbunSecretKeyFile` | _(required)_                          | File holding the Porkbun secret key, loaded through systemd `LoadCredential`                                                 |

### Units

The module installs two oneshot units, neither with a timer or `WantedBy`. The external trigger
writes `commitFile` and starts `deploy-converge.service`.

- `deploy-snapshot-refresh.service` runs privileged only to drop each end of an anonymous pipe to
  its own owner: it reads `sourceRepository` as its owner with system and global Git config disabled
  (`git bundle create - refs/heads/main`) and pipes directly into the service account's importer
  (`git bundle unbundle -`), updating `refs/heads/main` in `${stateDir}/snapshot.git` and nothing
  else. It takes nothing from the trigger (no `EnvironmentFile`), root opens no files in any
  user-writable directory, and its capabilities are limited to `CAP_SETUID`, `CAP_SETGID` and
  `CAP_DAC_READ_SEARCH`.
- `deploy-converge.service` requires and runs after `deploy-snapshot-refresh.service`, completely
  unprivileged as `user:group`, with no `EnvironmentFile`, an empty `CapabilityBoundingSet`, and
  `SystemCallFilter=@system-service ~@privileged`. It runs
  `deploy converge --infra file://${stateDir}/snapshot.git --commit-file <commitFile>` with
  systemd-provided credentials. `deploy` reads `commitFile` once as data, verifies `VALLEY_COMMIT`
  is a 40-character lowercase commit reachable from the snapshot's `refs/heads/main`, and determines
  the service exit status.

Both units run under strict filesystem protection (`ProtectSystem=strict`, `ProtectHome=true`,
`PrivateTmp=true`, `PrivateDevices=true`, `ProtectProc=invisible`, `RestrictNamespaces=true`,
`NoNewPrivileges=true`). `deploy-converge.service` also has `ReadWritePaths` for
`/nix/var/nix/daemon-socket` (so Nix builds reach the daemon), `TimeoutStartSec=2h`, and
`KillMode=mixed` (so `deploy` terminates and reaps its child group cleanly on stop).

### What the host must supply

- Provide the three credential files. They can stay `0400 root:root` because systemd loads them.
- Keep `sourceRepository` and `commitFile` outside `/home` and `/root`, which the services cannot
  see. The repository must have `refs/heads/main`.
- Have the trigger write `VALLEY_COMMIT` as a `NAME=value` line in `commitFile`, which must be
  readable by `user`. Then start `deploy-converge.service`.
- Create any custom `user` and `group` and give the user access to the Nix daemon. The default
  `nix.settings.allowed-users = [ "*" ]` already grants access.

## Locking

`deploy` takes a nonblocking exclusive `flock` on `DEPLOY_STATE_DIR/deploy.lock` before creating its
record, and exits 75 if another attempt holds it. The lock is only as good as the file staying put:
never remove `deploy.lock`. Mutating children (`skopeo copy`, `fly deploy`, `dns sync`) inherit the
locked descriptor, so the lock stays held while one of them lives, even if `deploy` itself dies.
`deploy` holds the lock through child termination, cleanup and the final record, then closes the
descriptor without unlocking it so any surviving child keeps the lock.

## Children

Every child runs in its own process group with a clean environment: the pinned tool path as `PATH`,
and private `HOME`, XDG and temporary directories inside the attempt. Nothing is inherited from the
invoking environment, so ambient credentials and endpoint overrides never apply. Each credential
reaches only the tools that use it:

| Credential   | Reaches                                                                                                                                                                                                                                                                 |
| ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Fly token    | `flyctl` as `FLY_API_TOKEN`; `skopeo` as an auth file with user `x` and the token as password, unlinked before the token is written and passed as an inherited descriptor that `REGISTRY_AUTH_FILE` names as `/proc/self/fd/<n>`, so no path to it outlives the attempt |
| Porkbun keys | `dns` as `PORKBUN_API_KEY` and `PORKBUN_SECRET_KEY`                                                                                                                                                                                                                     |

skopeo also gets a private policy accepting the local image, empty registry configuration (so no
system mirror or signature store applies), and a private `--tmpdir` to unpack the archive in.

Each child's command, outcome, stdout and stderr go to `runs/<attempt-id>/logs/<nn>-<label>.log`, in
a directory only the invoking user can read. While the child runs, deploy keeps in memory the first
64 MiB of each stream, plus room for a credential that starts before that point, and drains and
discards the rest, so the child never blocks; the log notes where output was cut. Before anything is
written, each credential is replaced by `[REDACTED]` in these forms: the literal value; its
URL-escaped forms (query and path escaping); its standard and URL-safe base64, padded and unpadded;
and the same base64 forms of `x:<credential>`, the registry auth. Redaction runs before the cut, so
no credential is written in part at the end of a stream. Each stream is redacted on its own: a
credential split between stdout and stderr, or encoded any other way, is not recognised. When a
child fails, is interrupted or times out, `deploy` prints its one-line reason and the path of its
log to stderr; nothing from the logs is copied into the record. A dry run's plan is also printed to
stdout, redacted the same way.

| Child           | Deadline                                             |
| --------------- | ---------------------------------------------------- |
| `nix build`     | 30 minutes                                           |
| `fly deploy`    | 10 minutes                                           |
| `skopeo copy`   | 10 minutes                                           |
| `git fetch`     | 5 minutes                                            |
| `dns sync`      | 5 minutes                                            |
| everything else | 60 seconds, and within verification's remaining time |

At a deadline or an interruption the child's group gets SIGTERM, then SIGKILL five seconds later,
and the child is reaped. Once `deploy` accepts a signal, no new child starts: accepting it and
starting a child exclude each other.

## Known limits

- A record left `uncertain` by SIGKILL during a read-only phase stays `uncertain`, though nothing
  changed.
- A cancellation that stops a child before it starts is reported as an interruption of that child.
- The started-Machine requirement is judged for the whole app, not per process group or service.
- The first snapshot after `fly deploy` is taken before the verify window starts, and does not count
  against it.

# Deploying from an infra revision with `cmd/deploy`

`deploy` runs one deployment from an exact commit of an infra repository: it builds and deploys a
Fly app, or converges the repository's DNS zones on Porkbun. Each invocation is an _attempt_. It
takes a lock, writes a record of what it does, and exits with a status that says how it ended.

The infra repository owns identities, rendering, builds and checks; `deploy` owns the layout it
reads from that repository and the steps it runs.

## Invocation

```sh
export DEPLOY_STATE_DIR=/var/lib/deploy

deploy app <name> --infra <checkout|git-url> --rev <commit> --fly-token-file <file>
deploy dns-sync   --infra <checkout|git-url> --rev <commit> \
  --porkbun-api-key-file <file> --porkbun-secret-key-file <file> [--dry-run]
```

| Input                                                 | Meaning                                                                                                                                                                                                                                                                                                                                                      |
| ----------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `--infra`                                             | A repository directory, or an `https://` or `file://` Git URL. URLs carry no credentials, query or fragment; SSH and plain HTTP are refused                                                                                                                                                                                                                  |
| `--rev`                                               | The full 40-character lowercase commit                                                                                                                                                                                                                                                                                                                       |
| `DEPLOY_STATE_DIR`                                    | An absolute directory holding `deploy.lock` and `runs/`, created if missing. No path component may be a symlink. It must belong to the running user and be writable by no one else; every ancestor must belong to that user or root (or the kernel's overflow uid for `/` inside a user namespace) and be writable by no one else unless sticky, like `/tmp` |
| `--fly-token-file`                                    | `app` only: the Fly API token                                                                                                                                                                                                                                                                                                                                |
| `--porkbun-api-key-file`, `--porkbun-secret-key-file` | `dns-sync` only: the Porkbun key pair                                                                                                                                                                                                                                                                                                                        |
| `--dry-run`                                           | `dns-sync` only: print the plan, change nothing                                                                                                                                                                                                                                                                                                              |

Credential files must be non-empty; terminal newlines are trimmed. Extra arguments and any other
flag, including target or environment overrides such as `--app`, are usage errors. `app` has no dry
run. `--help` and `--version` work as usual; the version is stamped at packaging time.

## Exit status

| Status        | Meaning                                             |
| ------------- | --------------------------------------------------- |
| 0             | completed, or a completed dry run                   |
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

`deploy` fetches `--rev` into a fresh private repository, checks that `FETCH_HEAD^{commit}` is
exactly that commit, and checks it out detached. Working-tree changes in a checkout never reach the
build. The commit is refused if it contains a gitlink or has no committed `flake.lock`. Git runs
with no configuration files, no credential helpers, no prompts, and only the file and HTTPS
transports.

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

| Field                             | Meaning                                                                                                              |
| --------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `schema_version`                  | `1`                                                                                                                  |
| `attempt_id`                      | the run directory's random name, also the image tag                                                                  |
| `verb`                            | `app` or `dns-sync`                                                                                                  |
| `name`                            | the `app` command's `<name>`, set when the record is created; null for `dns-sync`                                    |
| `dry_run`                         | whether `--dry-run` was given                                                                                        |
| `source`                          | `--infra`, as an absolute directory or the URL                                                                       |
| `requested_commit`                | `--rev`                                                                                                              |
| `resolved_commit`                 | what `FETCH_HEAD^{commit}` named                                                                                     |
| `lock_sha256`                     | SHA-256 of the committed `flake.lock`                                                                                |
| `config_sha256`                   | SHA-256 of each config file by name: `fly.json`, or `zones.json`                                                     |
| `platform`                        | the packaged Nix system                                                                                              |
| `runner_version`                  | `deploy --version`                                                                                                   |
| `image_digest`                    | the image manifest's `sha256:` digest                                                                                |
| `targets_before`, `targets_after` | the app as inspect observed it, and as last observed after apply; null for `dns-sync`                                |
| `started_at`, `finished_at`       | UTC RFC 3339 timestamps with nanoseconds                                                                             |
| `outcome`                         | `uncertain` if any phase is, else `failed` if any phase is, else `completed`; `uncertain` until the attempt finishes |
| `phases`                          | `name`, `outcome` and `reason` per phase                                                                             |
| `logs`                            | the child logs' directory, relative to the run: `logs`                                                               |

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

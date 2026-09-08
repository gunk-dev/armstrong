# Managing a UniFi site with `cmd/unifi`

`unifi` is the sibling of the DNS tool: it reads a JSON `#Site` document on
stdin — pipe it from `cue export` — and converges a UniFi Network site to
match. It talks to the official **Integration API** served by the console, not
the private controller API.

The exact request and response shapes, and which of them are confirmed against
a live console, are recorded separately in
[`unifi-api-notes.md`](./unifi-api-notes.md).

## Configuration

| Variable | Meaning |
| --- | --- |
| `UNIFI_URL` | Console base URL, e.g. `https://unifi.lan` |
| `UNIFI_API_KEY` | Integration API key, from Settings → Control Plane → Integrations |
| `UNIFI_SITE` | Site name; defaults to `Default` |
| `UNIFI_CA_FILE` | PEM bundle for the console's self-signed certificate |
| `UNIFI_INSECURE_TLS` | Set to `1` to skip certificate verification instead |
| `UNIFI_WIFI_*` | One variable per SSID, named by its `passphraseEnv` |

The console presents a self-signed certificate, so one of `UNIFI_CA_FILE` or
`UNIFI_INSECURE_TLS=1` is required. Prefer pinning the CA file.

## Commands

```sh
# Bootstrap an instance file from what the console already has.
unifi export > site.json

# Plan only. Exits 2 when anything would change, 1 on failure.
cue export ./unifi --out json -e site | unifi diff

# Converge.
cue export ./unifi --out json -e site | unifi sync --prune
```

`unifi sync --dry-run` prints exactly what `sync` would do and issues no writes;
it is the right first command to run against a console you have not synced
before.

`diff` distinguishes its two failure modes on purpose: **exit 2** means "the
plan is non-empty", **exit 1** means the command itself failed. A CI job that
gates on drift should treat only 2 as "config has drifted".

## The rules that make this safe

**Names are the identity.** Object ids are assigned by the console, so an
instance file cannot carry them and stay portable. Desired and actual objects
are matched by `name`. DNS policies have no name, so they are keyed by `type`
plus `domain`.

Renaming an object in the instance file therefore reads as "delete the old one,
create a new one", not "rename" — with `--prune` that is what will happen.

**`SYSTEM_DEFINED` objects are updated but never deleted.** The console creates
its own default network, and on some setups its own firewall policies. Declaring
one in the instance file updates its configurable fields in place; `--prune`
will not remove it, so a mistake in the instance file cannot delete the LAN out
from under you.

**`--prune` only acts on resource types the instance file declares.** If
`wifi` is an empty list, no SSID is deleted. This is deliberate: an instance
file that simply forgot a section would otherwise wipe every object of that
type on the first sync. To actually delete everything of one kind, remove the
entries individually rather than dropping the list.

**Secrets stay out of git.** `#WiFiSecurity` carries `passphraseEnv` — the
*name* of an environment variable — never the passphrase. `unifi export`
substitutes a generated variable name for each SSID's passphrase, so its output
is safe to commit. Passphrases and the API key are redacted from all output,
including API error responses, which can quote a rejected payload back.

**Reconciliation runs in dependency order:** networks → firewall zones → wifi,
firewall policies and DNS policies. Zones reference networks by name, and
policies reference zones by name, so the ids exist by the time they are needed.

## Zone-based firewall

The firewall endpoints only exist once the console has migrated off the legacy
firewall. Until then they answer `400
api.firewall.zone-based-firewall-not-configured`, and `unifi` prints

```
SKIP   firewall       zones+policies (zone-based firewall is not configured on this console)
```

then carries on with the rest of the site. Only that specific error code is
treated this way — any other firewall failure, including an expired key, is a
hard error, so a sync can never silently stop managing the firewall.

Because no console was available with the zone-based firewall enabled, that code
path is exercised only against the test fake. Run `--dry-run` first.

## Running it

`unifi sync` belongs on a **LAN host**, not in GitHub Actions: the console is
reachable only on the local network and presents a certificate no public runner
will trust. The intended deployment is a systemd timer on a NixOS box:

```
ExecStart = "${pkgs.writeShellScript "unifi-sync" ''
  ${cue}/bin/cue export ${./unifi} --out json -e site | ${armstrong}/bin/unifi sync --prune
''}";
```

with the API key and SSID passphrases supplied through
`systemd`'s `LoadCredential=` or an `EnvironmentFile=` outside the store.

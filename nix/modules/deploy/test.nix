# Boots a NixOS host running `deploy-converge.service` and
# `deploy-snapshot-refresh.service` to verify the module's own logic:
#
#   * `deploy-snapshot-refresh.service` refreshes the deploy-owned bare
#     snapshot across two users (where the source repository is mode 0700 owned
#     by another user and unreadable by `deploy`) without reading the trigger
#     file as environment;
#   * a trigger file containing `BASH_ENV=...` and extra names has no effect on
#     either the privileged refresh unit or the unprivileged converge unit;
#   * the real `deploy` binary refuses a malformed commit file (exit 2) and a
#     commit not reachable from the snapshot's `refs/heads/main` (exit 1)
#     before starting any attempt;
#   * root-only credential files reach `deploy` via LoadCredential and
#     `deploy`'s exit status (0, 1, 75) propagates as the unit's exit status.
{ pkgs, module }:

let
  # Stands in for `cmd/deploy` in the stub specialisation (where no network or
  # fake APIs exist): records its identity, arguments and credential contents
  # inside DEPLOY_STATE_DIR, fetches VALLEY_COMMIT from `--infra` with git
  # config disabled, and exits with the status in /run/stub-deploy-exit.
  stubDeploy = pkgs.writeShellApplication {
    name = "deploy";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.gawk
      pkgs.gitMinimal
    ];
    text = ''
      infra=""
      commit_file=""
      fly_file=""
      pb_key_file=""
      pb_secret_file=""
      args="$*"
      while [ $# -gt 0 ]; do
        case "$1" in
          --infra) infra="$2"; shift 2 ;;
          --commit-file) commit_file="$2"; shift 2 ;;
          --fly-token-file) fly_file="$2"; shift 2 ;;
          --porkbun-api-key-file) pb_key_file="$2"; shift 2 ;;
          --porkbun-secret-key-file) pb_secret_file="$2"; shift 2 ;;
          *) shift ;;
        esac
      done

      rev=$(awk -F= '$1 == "VALLEY_COMMIT" { print $2 }' "$commit_file")

      {
        echo "user=$(id -un):$(id -gn)"
        echo "state=$DEPLOY_STATE_DIR"
        echo "args=$args"
        echo "fly=$(cat "$fly_file")"
        echo "pb_key=$(cat "$pb_key_file")"
        echo "pb_secret=$(cat "$pb_secret_file")"
      } > "$DEPLOY_STATE_DIR/stub-log"

      work=$(mktemp -d)
      trap 'rm -rf "$work"' EXIT
      export GIT_CONFIG_NOSYSTEM=1
      export GIT_CONFIG_GLOBAL=/dev/null
      export GIT_TERMINAL_PROMPT=0
      export GIT_ALLOW_PROTOCOL=file:https
      git -C "$work" init --quiet --template=
      git -C "$work" fetch --quiet --no-tags --no-recurse-submodules "$infra" "$rev"
      resolved=$(git -C "$work" rev-parse --verify --quiet 'FETCH_HEAD^{commit}')
      test "$resolved" = "$rev"
      git -C "$work" -c advice.detachedHead=false checkout --quiet --detach "$rev"
      cp "$work/marker" "$DEPLOY_STATE_DIR/stub-marker"

      code=0
      if [ -r /run/stub-deploy-exit ]; then
        code=$(cat /run/stub-deploy-exit)
      fi
      exit "$code"
    '';
  };
in
pkgs.testers.runNixOSTest {
  name = "deploy-module";

  nodes.machine =
    { lib, ... }:
    {
      imports = [ module ];

      users.users.repoowner = {
        isSystemUser = true;
        group = "repoowner";
      };
      users.groups.repoowner = { };

      environment.systemPackages = [
        pkgs.gitMinimal
        pkgs.util-linux
      ];

      modules.deploy = {
        enable = true;
        sourceRepository = "/srv/infra.git";
        commitFile = "/run/valley/deploy.env";
        flyTokenFile = "/run/deploy-secrets/fly-token";
        porkbunApiKeyFile = "/run/deploy-secrets/porkbun-api-key";
        porkbunSecretKeyFile = "/run/deploy-secrets/porkbun-secret-key";
      };

      specialisation.stub.configuration = {
        modules.deploy.package = lib.mkForce stubDeploy;
      };
    };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    # No timer is installed and the service does not start on boot.
    machine.fail("systemctl cat deploy-converge.timer")
    assert machine.succeed("systemctl show -p ActiveState --value deploy-converge.service").strip() == "inactive"

    # Neither unit loads the trigger file as EnvironmentFile.
    assert machine.succeed("systemctl show -p EnvironmentFiles --value deploy-snapshot-refresh.service").strip() == ""
    assert machine.succeed("systemctl show -p EnvironmentFiles --value deploy-converge.service").strip() == ""

    # Root-only credential files (unreadable directly by deploy), and a
    # deploy-readable trigger directory.
    machine.succeed("install -d -m 0700 /run/deploy-secrets")
    machine.succeed("install -d -m 0755 /run/valley")
    machine.succeed("printf 'fly-secret-1\n' > /run/deploy-secrets/fly-token")
    machine.succeed("printf 'pb-key-1\n' > /run/deploy-secrets/porkbun-api-key")
    machine.succeed("printf 'pb-secret-1\n' > /run/deploy-secrets/porkbun-secret-key")
    machine.succeed("chmod 0400 /run/deploy-secrets/*")

    # Canary payload for BASH_ENV: writes the invoking uid into /var/lib/deploy,
    # which is writable by both deploy-snapshot-refresh and deploy-converge.
    machine.succeed(
        "cat > /run/pwn.sh <<'EOF'\n"
        "id -u > /var/lib/deploy/pwned\n"
        "EOF\n"
        "chmod 0755 /run/pwn.sh"
    )
    # Negative control: BASH_ENV actually fires when bash inherits it.
    machine.succeed("su -s /bin/sh deploy -c 'BASH_ENV=/run/pwn.sh bash -c true'")
    machine.succeed("test -e /var/lib/deploy/pwned")
    machine.succeed("rm -f /var/lib/deploy/pwned")

    # Source repository owned by repoowner, mode 0700 (unreadable by deploy).
    machine.succeed("install -d -o repoowner -g repoowner -m 0700 /srv/infra.git")
    machine.succeed(
        "su -s /bin/sh repoowner -c '"
        "git init -q --initial-branch=main /srv/infra.git && "
        "printf commit-one > /srv/infra.git/marker && "
        "printf \"{ outputs = _: { }; }\\n\" > /srv/infra.git/flake.nix && "
        "printf \"{\\\"nodes\\\":{\\\"root\\\":{}},\\\"root\\\":\\\"root\\\",\\\"version\\\":7}\\n\" > /srv/infra.git/flake.lock && "
        "git -C /srv/infra.git add marker flake.nix flake.lock && "
        "git -C /srv/infra.git -c user.name=t -c user.email=t@t commit -q -m first && "
        "git -C /srv/infra.git checkout -q -b side && "
        "printf commit-side > /srv/infra.git/marker && "
        "git -C /srv/infra.git add marker && "
        "git -C /srv/infra.git -c user.name=t -c user.email=t@t commit -q -m side && "
        "git -C /srv/infra.git checkout -q main'"
    )
    rev1 = machine.succeed("su -s /bin/sh repoowner -c 'git -C /srv/infra.git rev-parse refs/heads/main'").strip()
    side_rev = machine.succeed("su -s /bin/sh repoowner -c 'git -C /srv/infra.git rev-parse refs/heads/side'").strip()

    # Negative controls: deploy cannot read sourceRepository or host secrets directly.
    machine.fail("su -s /bin/sh deploy -c 'ls /srv/infra.git'")
    machine.fail("su -s /bin/sh deploy -c 'cat /run/deploy-secrets/fly-token'")

    # ---------------------------------------- real deploy: malformed commit files fail (exit 2) before anything starts
    for label, env_content in [
        ("empty commit", "BASH_ENV=/run/pwn.sh\nVALLEY_REPOSITORY=infra\nVALLEY_COMMIT=\n"),
        ("missing commit", "BASH_ENV=/run/pwn.sh\nVALLEY_REPOSITORY=infra\n"),
        ("duplicate commit", f"BASH_ENV=/run/pwn.sh\nVALLEY_COMMIT={rev1}\nVALLEY_COMMIT={rev1}\n"),
        ("malformed line", f"BASH_ENV=/run/pwn.sh\nnot-an-assignment\nVALLEY_COMMIT={rev1}\n"),
    ]:
        machine.succeed(f"printf {env_content!r} > /run/valley/deploy.env")
        machine.fail("systemctl start deploy-converge.service")
        assert machine.succeed("systemctl is-failed deploy-converge.service").strip() == "failed", label
        status = machine.succeed("systemctl show -p ExecMainStatus --value deploy-converge.service").strip()
        assert status == "2", f"{label}: got ExecMainStatus={status}, want 2"
        machine.fail("test -e /var/lib/deploy/runs")
        machine.fail("test -e /var/lib/deploy/pwned")
        machine.succeed("systemctl reset-failed deploy-converge.service")

    # ---------------------------------------- real deploy: unreachable commits refused (exit 1) before any attempt starts
    for label, env_content in [
        ("unknown commit", f"BASH_ENV=/run/pwn.sh\nEXTRA=ignored\nVALLEY_REPOSITORY=infra\nVALLEY_COMMIT={'1' * 40}\n"),
        ("commit not on main", f"BASH_ENV=/run/pwn.sh\nEXTRA=ignored\nVALLEY_REPOSITORY=infra\nVALLEY_COMMIT={side_rev}\n"),
    ]:
        machine.succeed(f"printf {env_content!r} > /run/valley/deploy.env")
        machine.fail("systemctl start deploy-converge.service")
        assert machine.succeed("systemctl is-failed deploy-converge.service").strip() == "failed", label
        status = machine.succeed("systemctl show -p ExecMainStatus --value deploy-converge.service").strip()
        assert status == "1", f"{label}: got ExecMainStatus={status}, want 1"
        machine.fail("test -e /var/lib/deploy/runs")
        machine.fail("test -e /var/lib/deploy/pwned")
        machine.succeed("systemctl reset-failed deploy-converge.service")

    # ---------------------------------------- real deploy: lock contention exits 75
    machine.succeed(f"printf 'BASH_ENV=/run/pwn.sh\nVALLEY_REPOSITORY=infra\nVALLEY_COMMIT={rev1}\n' > /run/valley/deploy.env")
    machine.succeed(
        "su -s /bin/sh deploy -c 'flock /var/lib/deploy/deploy.lock -c \"sleep 30\"' >/dev/null 2>&1 & "
        "pid=$!; sleep 0.5; "
        "! systemctl start deploy-converge.service; "
        "status=$(systemctl show -p ExecMainStatus --value deploy-converge.service); "
        "kill $pid 2>/dev/null || true; wait $pid 2>/dev/null || true; "
        "test \"$status\" = 75"
    )
    machine.fail("test -e /var/lib/deploy/pwned")
    machine.succeed("systemctl reset-failed deploy-converge.service")

    # ---------------------------------------- switch to stub specialisation for full end-to-end unit checks
    machine.succeed("/run/current-system/specialisation/stub/bin/switch-to-configuration test")

    # ---------------------------------------- cross-user refresh (commit 1) with BASH_ENV + extra names
    machine.succeed(
        f"printf 'BASH_ENV=/run/pwn.sh\nEXTRA_NAME=ignored\nVALLEY_REPOSITORY=infra\nVALLEY_COMMIT={rev1}\n' > /run/valley/deploy.env"
    )
    machine.succeed("systemctl start deploy-converge.service")
    machine.fail("test -e /var/lib/deploy/pwned")

    stub_log = machine.succeed("cat /var/lib/deploy/stub-log")
    assert "user=deploy:deploy" in stub_log, stub_log
    assert "state=/var/lib/deploy" in stub_log, stub_log
    assert "converge --infra file:///var/lib/deploy/snapshot.git --commit-file /run/valley/deploy.env" in stub_log, stub_log
    assert "fly=fly-secret-1" in stub_log, stub_log
    assert "pb_key=pb-key-1" in stub_log, stub_log
    assert "pb_secret=pb-secret-1" in stub_log, stub_log
    assert machine.succeed("cat /var/lib/deploy/stub-marker").strip() == "commit-one"
    assert machine.succeed("stat -c '%U:%G' /var/lib/deploy/snapshot.git").strip() == "deploy:deploy"

    # ---------------------------------------- refresh on new main commit
    machine.succeed(
        "su -s /bin/sh repoowner -c '"
        "printf commit-two > /srv/infra.git/marker && "
        "git -C /srv/infra.git add marker && "
        "git -C /srv/infra.git -c user.name=t -c user.email=t@t commit -q -m second'"
    )
    rev2 = machine.succeed("su -s /bin/sh repoowner -c 'git -C /srv/infra.git rev-parse refs/heads/main'").strip()
    machine.succeed(
        f"printf 'BASH_ENV=/run/pwn.sh\nVALLEY_REPOSITORY=infra\nVALLEY_COMMIT={rev2}\n' > /run/valley/deploy.env"
    )
    machine.succeed("systemctl start deploy-converge.service")
    machine.fail("test -e /var/lib/deploy/pwned")
    assert machine.succeed("cat /var/lib/deploy/stub-marker").strip() == "commit-two"

    # ---------------------------------------- exit status follows deploy
    for code in [1, 75]:
        machine.succeed(f"printf '{code}\n' > /run/stub-deploy-exit")
        machine.fail("systemctl start deploy-converge.service")
        assert machine.succeed("systemctl is-failed deploy-converge.service").strip() == "failed"
        status = machine.succeed("systemctl show -p ExecMainStatus --value deploy-converge.service").strip()
        assert status == str(code), f"got ExecMainStatus={status}, want {code}"
        machine.succeed("systemctl reset-failed deploy-converge.service")

    machine.succeed("printf '0\n' > /run/stub-deploy-exit")
    machine.succeed("systemctl start deploy-converge.service")
    assert machine.succeed("systemctl show -p ExecMainStatus --value deploy-converge.service").strip() == "0"
  '';
}

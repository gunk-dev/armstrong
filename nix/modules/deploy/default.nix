# Converge infra DNS and Fly apps from a commit file written by an external
# trigger.
#
# Snapshot refresh and convergence are separate systemd units:
#
#   * `deploy-snapshot-refresh.service` starts privileged only to drop each end
#     of an anonymous pipe to its own owner: the owner of `sourceRepository`
#     streams `git bundle create - refs/heads/main`, and `user:group` unbundles
#     from stdin into the deploy-owned bare repository and updates
#     `refs/heads/main`. It takes nothing from the trigger (no EnvironmentFile)
#     and root opens no files in any user-writable directory.
#
#   * `deploy-converge.service` requires and runs after
#     `deploy-snapshot-refresh.service`, completely unprivileged as
#     `user:group` with no EnvironmentFile. It runs
#     `deploy converge --infra file://<snapshot> --commit-file <path> ...`,
#     which reads `commitFile` once as data and requires `VALLEY_COMMIT` to be
#     an ancestor of the snapshot's `refs/heads/main` before anything starts.
#
# This module creates no secrets and installs no timer or trigger; another
# unit writes `commitFile` and starts `deploy-converge.service`.
{ armstrong }:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.modules.deploy;
  ourPackages = armstrong.packages.${pkgs.stdenv.hostPlatform.system};
  defaultStateDir = "/var/lib/deploy";
  snapshotDir = "${cfg.stateDir}/snapshot.git";
  commitFileStr = toString cfg.commitFile;

  # Runs as cfg.user:cfg.group: reads the git bundle from stdin into the
  # deploy-owned bare repository and updates refs/heads/main.
  snapshotImport = pkgs.writeShellApplication {
    name = "deploy-snapshot-import";
    runtimeInputs = [
      pkgs.gawk
      pkgs.gitMinimal
    ];
    text = ''
      export GIT_CONFIG_NOSYSTEM=1
      export GIT_CONFIG_GLOBAL=/dev/null
      export GIT_TERMINAL_PROMPT=0

      snap=${lib.escapeShellArg snapshotDir}
      if [ ! -d "$snap" ]; then
        git init --quiet --bare "$snap"
      fi
      head_oid=$(git -C "$snap" bundle unbundle - | awk '$2 == "refs/heads/main" { print $1 }')
      if [ -z "$head_oid" ]; then
        echo "deploy-snapshot-import: bundle has no refs/heads/main" >&2
        exit 1
      fi
      git -C "$snap" update-ref refs/heads/main "$head_oid"
    '';
  };

  # Privileged only so it can drop each end of the pipe to its own owner:
  # sourceRepository's owner creates the bundle on stdout; cfg.user:cfg.group
  # unbundles it from stdin into snapshotDir. Root opens no files in any
  # user-writable directory, and neither end inherits any trigger input.
  snapshotRefresh = pkgs.writeShellApplication {
    name = "deploy-snapshot-refresh";
    runtimeInputs = [
      pkgs.coreutils
      pkgs.getent
      pkgs.gitMinimal
      pkgs.util-linux
    ];
    text = ''
      src=${lib.escapeShellArg cfg.sourceRepository}
      owner_path="$src"
      if [ -d "$src/.git" ]; then
        owner_path="$src/.git"
      fi
      src_uid=$(stat -L -c '%u' "$owner_path")
      src_gid=$(stat -L -c '%g' "$owner_path")
      dst_uid=$(id -u ${lib.escapeShellArg cfg.user})
      dst_gid=$(getent group ${lib.escapeShellArg cfg.group} | cut -d: -f3)
      umask 077
      setpriv --reuid="$src_uid" --regid="$src_gid" --clear-groups --no-new-privs \
        env -i PATH=${lib.escapeShellArg "${pkgs.gitMinimal}/bin"} \
          GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 \
          git -C "$src" bundle create - refs/heads/main \
        | setpriv --reuid="$dst_uid" --regid="$dst_gid" --clear-groups --no-new-privs \
          env -i ${snapshotImport}/bin/deploy-snapshot-import
    '';
  };
in
{
  _file = ./default.nix;

  options.modules.deploy = {
    enable = lib.mkEnableOption "Converge infra DNS and Fly apps via deploy-converge.service";

    package = lib.mkOption {
      type = lib.types.package;
      default = ourPackages.deploy;
      defaultText = lib.literalExpression "armstrong.packages.\${system}.deploy";
      description = ''
        The `deploy` build to run. Defaults to armstrong's own packaged `deploy`
        with its pinned tool path.
      '';
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "deploy";
      description = ''
        System user `deploy-converge.service` runs as and owns the state
        directory and snapshot repository under. Created automatically when
        left at the default `"deploy"`.
      '';
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "deploy";
      description = ''
        Primary group for `user`. Created automatically when left at the
        default `"deploy"`.
      '';
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = defaultStateDir;
      description = ''
        Absolute directory for `DEPLOY_STATE_DIR` (`deploy.lock`, `apps/`,
        `runs/`, and the deploy-owned bare repository `snapshot.git`). Owned by
        `user:group` with mode `0700`.
      '';
    };

    sourceRepository = lib.mkOption {
      type = lib.types.str;
      example = "/srv/git/infra.git";
      description = ''
        Absolute path to the infra Git repository on disk (bare or non-bare)
        whose `refs/heads/main` is snapshotted before each converge. Need not
        be readable by `user`.
      '';
    };

    commitFile = lib.mkOption {
      type = lib.types.path;
      example = "/run/valley/deploy.env";
      description = ''
        Trigger file holding `NAME=value` lines including `VALLEY_COMMIT`,
        readable by `user` and outside `/home` and `/root`. Passed to
        `deploy converge --commit-file`, which reads it once as data and
        requires `VALLEY_COMMIT` to be an ancestor of the snapshot's
        `refs/heads/main`.
      '';
    };

    flyTokenFile = lib.mkOption {
      type = lib.types.path;
      example = lib.literalExpression ''config.age.secrets."fly-token".path'';
      description = ''
        File holding the Fly API token. Loaded via systemd `LoadCredential` so
        the host file may remain root-only (`0400`).
      '';
    };

    porkbunApiKeyFile = lib.mkOption {
      type = lib.types.path;
      example = lib.literalExpression ''config.age.secrets."porkbun-api-key".path'';
      description = ''
        File holding the Porkbun API key. Loaded via systemd `LoadCredential`.
      '';
    };

    porkbunSecretKeyFile = lib.mkOption {
      type = lib.types.path;
      example = lib.literalExpression ''config.age.secrets."porkbun-secret-key".path'';
      description = ''
        File holding the Porkbun secret key. Loaded via systemd `LoadCredential`.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = lib.hasPrefix "/" cfg.stateDir && cfg.stateDir != "/";
        message = "modules.deploy: stateDir must be an absolute path other than /.";
      }
      {
        assertion =
          !(lib.any (c: lib.hasInfix c cfg.stateDir) [
            " "
            "\t"
            "\n"
            "#"
            "?"
            "%"
          ]);
        message = "modules.deploy: stateDir must not contain whitespace, #, ? or %.";
      }
      {
        assertion = lib.hasPrefix "/" cfg.sourceRepository;
        message = "modules.deploy: sourceRepository must be an absolute path.";
      }
      {
        assertion =
          lib.hasPrefix "/" commitFileStr
          && !(lib.any (c: lib.hasInfix c commitFileStr) [
            " "
            "\t"
            "\n"
            "%"
          ]);
        message = "modules.deploy: commitFile must be an absolute path without whitespace or %.";
      }
    ];

    users.users = lib.mkIf (cfg.user == "deploy") {
      deploy = {
        isSystemUser = true;
        group = cfg.group;
        home = cfg.stateDir;
      };
    };

    users.groups = lib.mkIf (cfg.group == "deploy") {
      deploy = { };
    };

    # Ensure stateDir exists and belongs to user:group before
    # deploy-snapshot-refresh.service runs on a fresh host.
    systemd.tmpfiles.rules = [ "d ${cfg.stateDir} 0700 ${cfg.user} ${cfg.group} -" ];

    systemd.services.deploy-snapshot-refresh = {
      description = "Refresh deploy-owned snapshot of infra refs/heads/main";
      serviceConfig = {
        Type = "oneshot";
        UMask = "0077";
        SyslogIdentifier = "deploy-snapshot-refresh";
        TimeoutStartSec = "5m";

        ExecStart = "${snapshotRefresh}/bin/deploy-snapshot-refresh";
        ReadWritePaths = [ cfg.stateDir ];

        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectProc = "invisible";
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        NoNewPrivileges = true;
        RestrictAddressFamilies = [ "AF_UNIX" ];
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" ];
        # setpriv drops each end of the pipe to its own owner; CAP_DAC_READ_SEARCH
        # lets stat inspect a sourceRepository directory owned by another user.
        CapabilityBoundingSet = [
          "CAP_SETUID"
          "CAP_SETGID"
          "CAP_DAC_READ_SEARCH"
        ];
      };
    };

    systemd.services.deploy-converge = {
      description = "Converge infra DNS and Fly apps from commit file";
      requires = [ "deploy-snapshot-refresh.service" ];
      after = [
        "deploy-snapshot-refresh.service"
        "network-online.target"
        "nix-daemon.socket"
      ];
      wants = [ "network-online.target" ];

      serviceConfig = {
        Type = "oneshot";
        User = cfg.user;
        Group = cfg.group;
        UMask = "0077";
        SyslogIdentifier = "deploy-converge";
        # Each app may run two 30-minute nix builds plus upload, deploy and
        # verification.
        TimeoutStartSec = "2h";
        KillMode = "mixed";
        TimeoutStopSec = 30;

        Environment = [ "DEPLOY_STATE_DIR=${cfg.stateDir}" ];

        LoadCredential = [
          "fly-token:${cfg.flyTokenFile}"
          "porkbun-api-key:${cfg.porkbunApiKeyFile}"
          "porkbun-secret-key:${cfg.porkbunSecretKeyFile}"
        ];

        ExecStart = lib.concatStringsSep " " [
          "${cfg.package}/bin/deploy"
          "converge"
          "--infra"
          "file://${snapshotDir}"
          "--commit-file"
          commitFileStr
          "--fly-token-file"
          "%d/fly-token"
          "--porkbun-api-key-file"
          "%d/porkbun-api-key"
          "--porkbun-secret-key-file"
          "%d/porkbun-secret-key"
        ];

        # ProtectSystem=strict mounts /nix read-only, so grant access to the
        # nix-daemon socket (prefixed with '-' when the daemon is absent).
        ReadWritePaths = [
          "-/nix/var/nix/daemon-socket"
        ]
        ++ lib.optional (cfg.stateDir != defaultStateDir) cfg.stateDir;

        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectProc = "invisible";
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        NoNewPrivileges = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
        CapabilityBoundingSet = [ "" ];
      }
      // lib.optionalAttrs (cfg.stateDir == defaultStateDir) {
        StateDirectory = "deploy";
        StateDirectoryMode = "0700";
      };
    };
  };
}

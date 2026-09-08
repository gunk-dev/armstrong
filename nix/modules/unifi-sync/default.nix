# Keep a UniFi site converged to a CUE `#Site` instance file — or, by default,
# just say when it has drifted.
#
# The unit is the same pipeline a human runs by hand:
#
#   cue export ./instance --out json -e site | unifi <mode> [--prune]
#
# `instance` is a path *inside the consumer's flake*, so it is copied to the
# store at build time and the unit runs against the exact tree the host
# converged to. That is the property that makes the loop honest: there is no
# second copy of the desired state on the machine to drift from the repository.
# The schema it is checked against, and the binary that consumes the export,
# both come from this flake, so they cannot disagree.
#
# `mode` defaults to "diff", which never writes. In that mode drift is a
# failure: `unifi diff` exits 2 when the plan is non-empty, and the unit is
# allowed to fail on it so the drift is visible in `systemctl status` with the
# plan itself in the journal. Flipping to "sync" is the deliberate act that
# turns this from a detector into an actuator.
#
# `armstrong` is this flake, passed in by flake.nix, and is where `package` and
# `schema` get their defaults from. A consumer that wants a different build of
# either sets the options.
{ armstrong }:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.modules.unifi-sync;

  ourPackages = armstrong.packages.${pkgs.stdenv.hostPlatform.system};

  # cue locates a module by walking up from the *working directory* for
  # cue.mod/, not by looking above the package path it was handed. So the CUE
  # module is assembled here — a module identity, armstrong's schema package,
  # and the instance files — into one store tree that the pipeline cd's into.
  # Nothing armstrong owns is checked into the consumer's git tree:
  # `cue.mod/pkg/gunk.dev/armstrong` exists only in the store, built from the
  # same input as `package`.
  instanceTree = pkgs.runCommand "unifi-instance" { } ''
    mkdir -p "$out/cue.mod/pkg/gunk.dev"
    cp ${cfg.moduleFile} "$out/cue.mod/module.cue"
    ln -s ${cfg.schema}/cue.mod/pkg/gunk.dev/armstrong "$out/cue.mod/pkg/gunk.dev/armstrong"
    cp -r ${cfg.instance} "$out/instance"
  '';

  # The SSID passphrases. The unit gets them as an EnvironmentFile; a human
  # running the wrapper by hand has nothing that would set them, and a missing
  # passphrase is indistinguishable in the plan from a changed one — so read
  # the same file, if this run is privileged enough to, and say on stderr when
  # it is not rather than printing a WiFi line nobody should believe.
  #
  # Parsed rather than sourced: a passphrase is arbitrary text, and `. file`
  # would hand any $ or backtick in one to the shell.
  loadSecrets = lib.optionalString (cfg.secretsFile != null) ''
    if [ -r ${lib.escapeShellArg cfg.secretsFile} ]; then
      while IFS= read -r line || [ -n "$line" ]; do
        case "$line" in
          "" | "#"*) continue ;;
          *=*) export "''${line%%=*}=''${line#*=}" ;;
        esac
      done < ${lib.escapeShellArg cfg.secretsFile}
    else
      echo "$0: cannot read ${toString cfg.secretsFile}; re-run with sudo," >&2
      echo "  or every SSID will show as needing a passphrase update." >&2
    fi
  '';

  # The pipeline, shared by the unit and the `unifi-plan` wrapper. Both go
  # through the same builder so a human's dry run cannot diverge from what the
  # timer actually does.
  #
  # `unifi diff` exits 2 for "changes are needed" and 1 for "the command
  # failed"; the caller decides what to do with that, so nothing is swallowed
  # here.
  pipeline =
    {
      name,
      args,
      preamble ? "",
    }:
    pkgs.writeShellApplication {
      inherit name;
      runtimeInputs = [
        pkgs.cue
        cfg.package
      ];
      text = ''
        export UNIFI_URL=${lib.escapeShellArg cfg.consoleUrl}
        export UNIFI_SITE=${lib.escapeShellArg cfg.site}
        ${lib.optionalString (cfg.caFile != null) "export UNIFI_CA_FILE=${lib.escapeShellArg cfg.caFile}"}
        ${lib.optionalString cfg.insecureTls "export UNIFI_INSECURE_TLS=1"}

        # The API key is never put in the unit's environment, where anything
        # able to read /proc could lift it. Under systemd it arrives as a
        # credential; a human running the wrapper reads the secret file
        # directly, which is why that path is a fallback rather than the rule.
        if [ -n "''${CREDENTIALS_DIRECTORY:-}" ] && [ -r "$CREDENTIALS_DIRECTORY/api-key" ]; then
          UNIFI_API_KEY=$(cat "$CREDENTIALS_DIRECTORY/api-key")
        else
          UNIFI_API_KEY=$(cat ${lib.escapeShellArg cfg.apiKeyFile})
        fi
        export UNIFI_API_KEY

        ${preamble}

        cd ${instanceTree}
        cue export ./instance --out json -e site \
          | unifi ${lib.escapeShellArgs args}
      '';
    };

  syncScript = pipeline {
    name = "unifi-sync-run";
    args = [ cfg.mode ] ++ lib.optional cfg.prune "--prune";
  };

  # What a human runs to see the plan. Always `sync --dry-run`, whatever `mode`
  # is set to, so asking "what would change?" can never change anything.
  planScript = pipeline {
    name = "unifi-plan";
    preamble = loadSecrets;
    args = [
      "sync"
      "--dry-run"
    ]
    ++ lib.optional cfg.prune "--prune";
  };
in
{
  _file = ./default.nix;

  options.modules.unifi-sync = {
    enable = lib.mkEnableOption "Reconcile the UniFi site against a CUE #Site instance file";

    package = lib.mkOption {
      type = lib.types.package;
      default = ourPackages.unifi;
      defaultText = lib.literalExpression "armstrong.packages.\${system}.unifi";
      description = ''
        The `unifi` build to run. Defaults to armstrong's own package — the
        same derivation `nix run github:gunk-dev/armstrong#unifi` produces.
      '';
    };

    consoleUrl = lib.mkOption {
      type = lib.types.str;
      example = "https://192.168.1.1";
      description = ''
        Base URL of the UniFi console. The Integration API lives under
        `/proxy/network/integration/v1` on it.
      '';
    };

    site = lib.mkOption {
      type = lib.types.str;
      default = "Default";
      description = "UniFi site name, as shown in the console's site list.";
    };

    instance = lib.mkOption {
      type = lib.types.path;
      example = lib.literalExpression "./net/unifi";
      description = ''
        The CUE package directory holding `site: schema.#Site & {…}`. It is a
        path inside the consumer's flake, so the instance is copied into the
        store at build time and the unit runs against the exact tree this
        generation was built from — never a working copy on disk that could
        have moved underneath it.
      '';
    };

    moduleFile = lib.mkOption {
      type = lib.types.path;
      default = ./module.cue;
      defaultText = lib.literalExpression "./module.cue (armstrong's own)";
      description = ''
        The `cue.mod/module.cue` to assemble the store module around: a module
        identity and a `language: version`, the one piece that is not
        armstrong's schema. It is a separate option because `cue` finds a
        module by walking up from the working directory, so the module has to
        be rebuilt in the store around `instance` rather than pointed at.

        The default is a neutral identity that works for any instance
        directory. Set it to the consumer repository's own
        `cue.mod/module.cue` to evaluate the instance under exactly the module
        identity and language version the repository uses elsewhere.
      '';
    };

    schema = lib.mkOption {
      type = lib.types.package;
      default = ourPackages.schema;
      defaultText = lib.literalExpression "armstrong.packages.\${system}.schema";
      description = ''
        A derivation whose output holds `cue.mod/pkg/gunk.dev/armstrong`,
        linked into the assembled module so `instance` can import
        `gunk.dev/armstrong/schema`. Defaults to the same armstrong revision
        `package` is built from, which is what keeps the schema the instance is
        validated against and the binary that consumes the export in step.
      '';
    };

    apiKeyFile = lib.mkOption {
      type = lib.types.path;
      example = lib.literalExpression ''config.age.secrets."unifi-api-key".path'';
      description = ''
        File holding the Integration API key (Settings → Control Plane →
        Integrations), typically an agenix or sops-nix path. It reaches the
        unit as a systemd credential, not through the environment.
      '';
    };

    secretsFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = lib.literalExpression ''config.age.secrets."unifi-wifi.env".path'';
      description = ''
        `EnvironmentFile` holding one `NAME=value` line per SSID passphrase,
        named by each `#WiFi`'s `passphraseEnv`. Required whenever the instance
        file declares a non-OPEN SSID: `unifi` fails rather than write an empty
        passphrase.
      '';
    };

    caFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      example = lib.literalExpression "./unifi-console-ca.pem";
      description = ''
        PEM bundle for the console's self-signed certificate
        (`UNIFI_CA_FILE`). Mutually exclusive with `insecureTls`, and exactly
        one of the two has to be set: a console that serves a certificate no
        public CA signed will not be reached without deciding which.
      '';
    };

    insecureTls = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Skip certificate verification (`UNIFI_INSECURE_TLS=1`) instead of
        pinning a CA. Defensible only because the console is a fixed address on
        the same LAN, and worth replacing with `caFile` once the console's
        certificate has been exported. Mutually exclusive with `caFile`.
      '';
    };

    mode = lib.mkOption {
      type = lib.types.enum [
        "diff"
        "sync"
      ];
      default = "diff";
      description = ''
        `diff` never writes: it prints the plan and exits 2 if anything would
        change, which leaves the unit failed and the drift visible. `sync`
        applies the plan and logs what it did.
      '';
    };

    prune = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Delete USER_DEFINED objects the instance file does not declare. In
        `diff` mode this only adds the deletions to the printed plan.
        `SYSTEM_DEFINED` objects are never deleted, and resource types the
        instance file leaves as an empty list are never pruned.
      '';
    };

    onSuccessOf = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "nixos-upgrade" ];
      description = ''
        Unit names (no `.service` suffix) whose success triggers a run, so the
        site is reconciled the moment the host finishes converging rather than
        up to a day later. Success only: a converge that failed has not
        necessarily deployed the tree this instance file belongs to.

        Equivalent to setting `systemd.services.<name>.onSuccess =
        [ "unifi-sync.service" ]` on the consumer's own converge unit.
      '';
    };

    after = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "network-online.target" ];
      description = ''
        `After=` for the unit. The default only orders the run behind the
        network being up; it does not pull that target in, because on a host
        that never reaches it a manual `systemctl start` should still run.
      '';
    };

    wantedBy = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "multi-user.target" ];
      description = ''
        `WantedBy=` for the unit. Empty by default: the timer and
        `onSuccessOf` are what normally start it, and reconciling the console
        on every boot is rarely what a host wants.
      '';
    };

    checkAt = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = "05:30";
      description = ''
        `OnCalendar` expression for the daily drift check, or `null` to install
        no timer. This is the backstop for changes made in the console's own
        UI, which no converge would ever notice.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    # A console reached over https serves a certificate no public CA signed,
    # so one of the two has to be chosen explicitly. Defaulting either way
    # would be wrong: silently pinning nothing is the failure mode a TLS
    # option must never have, and silently requiring a CA would just break.
    # Over plain http (the VM test's fake console) neither applies.
    assertions = [
      {
        assertion = !(lib.hasPrefix "https://" cfg.consoleUrl) || ((cfg.caFile != null) != cfg.insecureTls);
        message = ''
          modules.unifi-sync: consoleUrl is https, so set exactly one of
          `caFile` (pin the console's certificate) or `insecureTls = true`
          (knowingly skip verification).
        '';
      }
      {
        assertion = !(cfg.caFile != null && cfg.insecureTls);
        message = "modules.unifi-sync: caFile and insecureTls are mutually exclusive.";
      }
    ];

    environment.systemPackages = [ planScript ];

    systemd.services = lib.mkMerge [
      {
        unifi-sync = {
          description = "Reconcile UniFi site ${cfg.site} at ${cfg.consoleUrl} (${cfg.mode})";
          inherit (cfg) after wantedBy;
          serviceConfig = {
            Type = "oneshot";
            ExecStart = "${syncScript}/bin/unifi-sync-run";
            SyslogIdentifier = "unifi-sync";
            TimeoutStartSec = 300;

            # The API key: readable only by this unit, never in its
            # environment, never in the store.
            LoadCredential = [ "api-key:${cfg.apiKeyFile}" ];

            # DynamicUser gives the run no identity that outlives it, which is
            # all it needs: it talks to one HTTPS endpoint and writes nothing.
            DynamicUser = true;
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
            # IP to reach the console; AF_UNIX because the resolver and the
            # journal connection both want it.
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
          // lib.optionalAttrs (cfg.secretsFile != null) {
            EnvironmentFile = cfg.secretsFile;
          };
        };
      }

      # Reconcile right after a converge lands, so a merged change to the
      # instance file reaches the console in the same breath as the rebuild
      # that carried it.
      (lib.genAttrs cfg.onSuccessOf (_: {
        onSuccess = [ "unifi-sync.service" ];
      }))
    ];

    # The backstop. A converge only fires when the repository moves; drift here
    # usually comes from the other direction — somebody changing the site in
    # the console's UI — which nothing but a clock would ever notice.
    systemd.timers = lib.mkIf (cfg.checkAt != null) {
      unifi-sync = {
        description = "Daily UniFi drift check";
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnCalendar = cfg.checkAt;
          RandomizedDelaySec = "10m";
          Persistent = true;
        };
      };
    };
  };
}

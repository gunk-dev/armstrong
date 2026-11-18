{
  description = "armstrong — shared CUE schemas and platform CLIs for gunk-dev";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];

      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # Stamped into the schema output so a consumer can tell which armstrong
      # revision its cue.mod/pkg tree came from.
      rev = self.rev or self.dirtyRev or "unknown";
      version = self.shortRev or self.dirtyShortRev or "unknown";

      # Hash of the vendored Go module tree. Update with the hash nix prints
      # after a go.mod/go.sum change.
      vendorHash = "sha256-7K17JaXFsjf163g5PXCb5ng2gYdotnZ2IDKk8KFjNj0=";

      # Every Go derivation builds from the same source and module set. The
      # vendored-module derivation is named without the revision, so it has one
      # fixed-output store path: the modules are fetched once and shared by the
      # packages and the checks, across commits, until vendorHash changes.
      mkGo =
        pkgs: args:
        pkgs.buildGoModule (
          {
            inherit version vendorHash;
            src = ./.;
            overrideModAttrs = _: { name = "armstrong-go-modules"; };
          }
          // args
        );

      # The tests run once, in checks.go-test, rather than again in each
      # package's checkPhase.
      mkTool =
        pkgs: name: extra:
        mkGo pkgs (
          {
            pname = name;
            subPackages = [ "cmd/${name}" ];
            doCheck = false;
            meta = {
              description = "armstrong ${name} CLI";
              mainProgram = name;
              homepage = "https://github.com/gunk-dev/armstrong";
            };
          }
          // extra
        );

      # The deploy runner. Its children run with exactly the PATH stamped in
      # here, so it uses these locked tools and never the host's.
      mkDeploy =
        pkgs: dns:
        mkTool pkgs "deploy" {
          ldflags = [
            "-X main.version=${version}"
            "-X main.nixSystem=${pkgs.stdenv.hostPlatform.system}"
            "-X main.storeDir=${builtins.storeDir}"
            "-X main.toolPath=${
              pkgs.lib.makeBinPath [
                pkgs.gitMinimal
                pkgs.nix
                pkgs.cue
                pkgs.flyctl
                pkgs.skopeo
                dns
              ]
            }"
          ];
          doInstallCheck = true;
          installCheckPhase = ''
            $out/bin/deploy --version | grep -qF ${version}
          '';
        };

      # A derivation that runs `cmd` in a configured Go build environment and
      # succeeds by producing an empty $out. cue is on PATH because the unifi
      # tests shell out to it to check the schema's own rules; the deploy tests
      # drive real git and cue, and check deploy's invocations against the
      # skopeo and flyctl it pins. Tests skip what is missing.
      mkGoCheck =
        pkgs: name: cmd:
        mkGo pkgs {
          pname = "armstrong-${name}";
          nativeBuildInputs = [
            pkgs.cue
            pkgs.gitMinimal
            pkgs.skopeo
            pkgs.flyctl
          ];
          buildPhase = ''
            runHook preBuild
            export CUE_CACHE_DIR=$TMPDIR/cue
            ${cmd}
            runHook postBuild
          '';
          doCheck = false;
          installPhase = "touch $out";
        };

      # .github alone, so the workflow linters rebuild only when it changes.
      githubSrc = nixpkgs.lib.fileset.toSource {
        root = ./.;
        fileset = ./.github;
      };

      # The tracked Markdown alone, for prose-format.
      markdownSrc = nixpkgs.lib.fileset.toSource {
        root = ./.;
        fileset = nixpkgs.lib.fileset.fileFilter (file: file.hasExt "md") ./.;
      };
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          unifi = mkTool pkgs "unifi" { };
          dns = mkTool pkgs "dns" { };
        in
        {
          inherit unifi dns;
          deploy = mkDeploy pkgs dns;
          default = unifi;

          # The schema/ directory laid out as a CUE module package path, so a
          # consumer can assemble its cue.mod/pkg by linking
          # ${schema}/cue.mod/pkg/gunk.dev/armstrong instead of vendoring .cue
          # files into git.
          schema = pkgs.runCommand "armstrong-schema-${version}" { } ''
            dir=$out/cue.mod/pkg/gunk.dev/armstrong/schema
            mkdir -p "$dir"
            cp ${./schema}/*.cue "$dir"/
            echo ${rev} > $out/cue.mod/pkg/gunk.dev/armstrong/VERSION
          '';
        }
      );

      # The NixOS actuator for `schema.#Site`: a hardened oneshot that runs
      # `cue export <instance> -e site | unifi <diff|sync>`, plus a daily drift
      # timer and a `unifi-plan` wrapper. `self` is threaded in so `package`
      # and `schema` default to this flake's own outputs, with no `inputs`
      # argument required of the consumer's module system.
      nixosModules = {
        unifi-sync = import ./nix/modules/unifi-sync { armstrong = self; };
        default = self.nixosModules.unifi-sync;
      };

      checks = forAllSystems (pkgs: {
        inherit (self.packages.${pkgs.stdenv.hostPlatform.system})
          unifi
          dns
          deploy
          schema
          ;

        go-vet = mkGoCheck pkgs "go-vet" "go vet ./...";
        go-test = mkGoCheck pkgs "go-test" "go test ./...";

        cue-vet = pkgs.runCommand "armstrong-cue-vet" { nativeBuildInputs = [ pkgs.cue ]; } ''
          cp -R ${self} src
          chmod -R u+w src
          cd src
          export CUE_CACHE_DIR=$TMPDIR/cue
          cue vet ./schema
          touch $out
        '';

        # Exercises the consumer path documented in the README: assemble a
        # cue.mod/pkg from packages.schema and evaluate a real instance file
        # against it.
        schema-consumer =
          pkgs.runCommand "armstrong-schema-consumer" { nativeBuildInputs = [ pkgs.cue ]; }
            ''
              cp -R ${./examples/unifi} src
              chmod -R u+w src
              cd src
              mkdir -p cue.mod/pkg/gunk.dev
              cat > cue.mod/module.cue <<EOF
              module: "example.com/consumer"
              language: version: "v0.9.2"
              EOF
              ln -s ${
                self.packages.${pkgs.stdenv.hostPlatform.system}.schema
              }/cue.mod/pkg/gunk.dev/armstrong cue.mod/pkg/gunk.dev/armstrong
              export CUE_CACHE_DIR=$TMPDIR/cue
              cue export . --out json -e site > $out
            '';

        # The module's VM test. It boots a host running the real unit against a
        # fake console, so it needs KVM: `nix flake check` on a machine or
        # runner without /dev/kvm cannot build it. GitHub-hosted
        # `ubuntu-latest` runners do have it.
        unifi-sync-vm = import ./nix/modules/unifi-sync/test.nix {
          inherit pkgs;
          module = self.nixosModules.unifi-sync;
        };

        # The module imported into a scratch NixOS configuration, so a change
        # that only breaks on evaluation (a bad option type, a reference to an
        # `inputs` argument the consumer does not pass) fails here rather than
        # in the consumer's repository. Cheap: it forces the rendered unit and
        # the assertions, not a whole system closure.
        unifi-sync-eval =
          let
            scratch = nixpkgs.lib.nixosSystem {
              modules = [
                self.nixosModules.unifi-sync
                {
                  nixpkgs.hostPlatform = pkgs.stdenv.hostPlatform.system;
                  system.stateVersion = "25.05";
                  boot.loader.grub.enable = false;
                  fileSystems."/" = {
                    device = "/dev/disk/by-label/nixos";
                    fsType = "ext4";
                  };

                  modules.unifi-sync = {
                    enable = true;
                    consoleUrl = "https://198.51.100.1";
                    insecureTls = true;
                    instance = ./nix/modules/unifi-sync/test-instance;
                    apiKeyFile = "/run/secrets/unifi-api-key";
                    secretsFile = "/run/secrets/unifi-wifi.env";
                    onSuccessOf = [ "nixos-upgrade" ];
                    mode = "sync";
                    prune = true;
                  };
                }
              ];
            };
            failed = builtins.filter (a: !a.assertion) scratch.config.assertions;
          in
          assert nixpkgs.lib.assertMsg (failed == [ ]) (
            "unifi-sync-eval: " + nixpkgs.lib.concatMapStringsSep "; " (a: a.message) failed
          );
          pkgs.runCommand "armstrong-unifi-sync-eval" { } ''
            unit=${scratch.config.systemd.units."unifi-sync.service".unit}/unifi-sync.service
            grep -q 'ExecStart=.*/bin/unifi-sync-run' "$unit"
            grep -q 'DynamicUser=true' "$unit"
            # sync mode writes its pre-apply snapshots under the state directory.
            grep -q 'StateDirectory=unifi-sync' "$unit"
            grep -q 'LoadCredential=api-key:/run/secrets/unifi-api-key' "$unit"
            # onSuccessOf reached the consumer's own unit.
            grep -q 'OnSuccess=unifi-sync.service' \
              ${scratch.config.systemd.units."nixos-upgrade.service".unit}/nixos-upgrade.service
            cp "$unit" $out
          '';

        yamllint = pkgs.runCommand "armstrong-yamllint" { nativeBuildInputs = [ pkgs.yamllint ]; } ''
          yamllint -d "{extends: default, rules: {line-length: disable, truthy: disable}}" \
            ${githubSrc}/.github/workflows
          touch $out
        '';

        zizmor = pkgs.runCommand "armstrong-zizmor" { nativeBuildInputs = [ pkgs.zizmor ]; } ''
          zizmor --offline ${githubSrc}
          touch $out
        '';

        # The platform offers no shared definition of the floor's prose-format check, so its command is restated here.
        prose-format =
          pkgs.runCommand "armstrong-prose-format" { nativeBuildInputs = [ pkgs.prettier ]; }
            ''
              cd ${markdownSrc}
              find . -name '*.md' -exec prettier --prose-wrap always --print-width 100 \
                --embedded-language-formatting off --check {} +
              touch $out
            '';

        nixfmt = pkgs.runCommand "armstrong-nixfmt" { nativeBuildInputs = [ pkgs.nixfmt ]; } ''
          find ${self} -name '*.nix' -print0 | xargs -0 nixfmt --check
          touch $out
        '';
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.cue
            pkgs.nixfmt
            pkgs.gh
          ];
        };
      });

      # treefmt around nixfmt: `nix fmt` with no arguments formats the tree.
      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);
    };
}

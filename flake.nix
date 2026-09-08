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

      mkTool =
        pkgs: name:
        pkgs.buildGoModule {
          pname = name;
          inherit version vendorHash;
          src = ./.;
          subPackages = [ "cmd/${name}" ];
          meta = {
            description = "armstrong ${name} CLI";
            mainProgram = name;
            homepage = "https://github.com/gunk-dev/armstrong";
          };
        };

      # A derivation that runs `cmd` in a configured Go build environment and
      # succeeds by producing an empty $out.
      mkGoCheck =
        pkgs: name: cmd:
        pkgs.buildGoModule {
          pname = "armstrong-${name}";
          inherit version vendorHash;
          src = ./.;
          buildPhase = ''
            runHook preBuild
            ${cmd}
            runHook postBuild
          '';
          doCheck = false;
          installPhase = "touch $out";
        };
    in
    {
      packages = forAllSystems (
        pkgs:
        let
          unifi = mkTool pkgs "unifi";
        in
        {
          inherit unifi;
          dns = mkTool pkgs "dns";
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

      checks = forAllSystems (pkgs: {
        inherit (self.packages.${pkgs.stdenv.hostPlatform.system}) unifi dns schema;

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

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}

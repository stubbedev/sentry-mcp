{
  description = "MCP server for self-hosted Sentry (Go, with TOON output)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      # Version tracks the npm wrapper / git tag automatically: it is read from
      # package.json, which `npm run release:*` bumps. No manual edit needed.
      version = (builtins.fromJSON (builtins.readFile ./package.json)).version;

      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        sentry-mcp = pkgs.buildGoModule {
          pname = "sentry-mcp";
          inherit version;
          src = self;

          # vendorHash is kept current by .github/workflows/flake.yml on any
          # change to go.mod / go.sum. If you bump deps locally, set this to
          # pkgs.lib.fakeHash, run `nix build`, and paste the reported hash.
          vendorHash = "sha256-BZypMNj8pLnDL8baNKAesU6FYyRZw6a2xq3lp61PyeY=";

          # Version comes from the embedded package.json at runtime, so no
          # -X main.Version wiring is needed here.
          ldflags = [ "-s" "-w" ];
          doCheck = true;

          meta = {
            description = "MCP server for self-hosted Sentry (Go, with TOON output)";
            homepage = "https://github.com/stubbedev/sentry-mcp";
            license = pkgs.lib.licenses.mit;
            mainProgram = "sentry-mcp";
          };
        };
        default = sentry-mcp;
      });

      apps = forAllSystems (pkgs: rec {
        sentry-mcp = {
          type = "app";
          program = "${self.packages.${pkgs.system}.sentry-mcp}/bin/sentry-mcp";
        };
        default = sentry-mcp;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.gopls pkgs.gotools ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixpkgs-fmt);
    };
}

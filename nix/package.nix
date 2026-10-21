{
  perSystem = { lib, pkgs, self', ... }: {
    packages = {
      sentry-mcp = pkgs.buildGo127Module {
        pname = "sentry-mcp";
        # Version tracks the npm wrapper / git tag automatically: it is read from
        # package.json, which `npm run release:*` bumps. No manual edit needed.
        inherit (lib.importJSON ../package.json) version;

        src = lib.fileset.toSource {
          root = ../.;
          fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
            lib.fileset.unions [
              ../flake.nix
              ../flake.lock
              ../nix
            ]
          );
        };

        # vendorHash is kept current by .github/workflows/flake.yml on any
        # change to go.mod / go.sum. If you bump deps locally, set this to
        # pkgs.lib.fakeHash, run `nix build`, and paste the reported hash.
        vendorHash = "sha256-h/psj9Ww4Bi/C1dnnwAzdYR/sXKz9x7C7uP2g1SBbnY=";

        # Version comes from the embedded package.json at runtime, so no
        # -X main.Version wiring is needed here.
        ldflags = [ "-s" "-w" ];
        doCheck = true;

        meta = {
          description = "MCP server for self-hosted Sentry (Go, with TOON output)";
          homepage = "https://github.com/stubbedev/sentry-mcp";
          license = lib.licenses.mit;
          mainProgram = "sentry-mcp";
        };
      };
      default = self'.packages.sentry-mcp;
    };

    apps = {
      sentry-mcp = {
        type = "app";
        program = lib.getExe self'.packages.sentry-mcp;
      };
      default = self'.apps.sentry-mcp;
    };
  };
}

{
  description = "MCP server for self-hosted Sentry (Go, with TOON output)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };
  };

  # CI (.github/workflows/flake.yml) pushes the built x86_64-linux closure to
  # this Attic cache. Consumers get it automatically with --accept-flake-config,
  # or by copying these two lines into their own nix.conf. Cache is public, so
  # no auth/token is needed to pull.
  nixConfig = {
    extra-substituters = [ "https://nix.stubbe.dev/default" ];
    extra-trusted-public-keys = [ "default:9P4FePqHV1rGv5NDBun0GN26y83pcaaMr/NHZrxKaac=" ];
  };

  outputs = inputs @ { flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      imports = [
        ./nix/package.nix
        ./nix/shell.nix
      ];
    };
}

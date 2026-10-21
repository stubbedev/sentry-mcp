{
  perSystem = { pkgs, self', ... }: {
    devShells.default = pkgs.mkShell {
      inputsFrom = [ self'.packages.default ];
      packages = [ pkgs.gopls pkgs.gotools ];
    };

    formatter = pkgs.nixpkgs-fmt;
  };
}

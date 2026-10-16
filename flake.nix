{
  description = "Eve: fire-and-forget iOS builds on your own Mac — TestFlight, device installs and checks";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["aarch64-darwin" "x86_64-linux" "aarch64-linux"];
    forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
  in {
    packages = forAll (pkgs: {
      default = pkgs.callPackage ./nix/package.nix {};
      eve = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
    });

    # home-manager module for the build host (Quartz): services.eve (nix/home-manager-module.nix).
    homeManagerModules.default = import ./nix/home-manager-module.nix self;

    checks = forAll (pkgs: {
      eve = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
    });

    devShells = forAll (pkgs: {
      default = pkgs.mkShell {packages = [pkgs.go pkgs.gopls pkgs.gofumpt pkgs.golangci-lint];};
    });

    formatter = forAll (pkgs: pkgs.alejandra);
  };
}

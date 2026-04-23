{
  description = "CUProxy – IPP proxy that stitches banner pages onto print jobs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = pkgs.buildGoModule {
          name = "cuproxy";
          src = ./.;

          vendorHash = "sha256-tuSDQ1FAMn91GAkuoAQcPC1rX5sPl8MgijY5TrCV+ek=";

          meta = with pkgs.lib; {
            description = "IPP proxy that prepends/appends banner pages to print jobs";
            homepage = "https://github.com/tuupke/cuproxy";
            mainProgram = "cuproxy";
            platforms = platforms.linux;
          };
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools
            typst
          ];
        };
      }
    )
    // {
      nixosModules.default = import ./nix/module.nix self;
    };
}

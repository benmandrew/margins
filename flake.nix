{
  description = "thesis — builds a PDF per commit of a Typst thesis and serves diffs between versions";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" ]
      (system:
        let
          pkgs = import nixpkgs { inherit system; };

          # Not in nixpkgs. Built from the crates.io tarball, which ships its
          # Cargo.lock, so the dependency set is the one upstream tested.
          typdiff = pkgs.rustPlatform.buildRustPackage rec {
            pname = "typdiff";
            version = "0.1.4";
            src = pkgs.fetchCrate {
              inherit pname version;
              hash = "sha256-lb0xpYEameXDocIKGPs8ObiL9dhRAQXmEE1Z1Bl2LuM=";
            };
            cargoHash = "sha256-O/C3yIgHJrJXb6/8tr/KLfQzOu9YsfYPq71WHXjGUmY=";
            meta.mainProgram = "typdiff";
          };

          # External programs the service shells out to at run time. Typst is
          # absent on purpose: the service downloads the exact version each
          # commit pins (see .typst-review.toml in the brief).
          runtimeDeps = [
            pkgs.git
            pkgs.diff-pdf
            pkgs.poppler-utils
            typdiff
          ];
        in
        {
          packages = {
            inherit typdiff;
          };

          devShells.default = pkgs.mkShell {
            packages = runtimeDeps ++ [
              pkgs.go
              pkgs.gopls
              pkgs.golangci-lint
              pkgs.sqlite
              # For writing and spiking fixtures only; never used by the service.
              pkgs.typst
            ];
          };
        });
}

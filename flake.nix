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
          lib = pkgs.lib;

          # Not in nixpkgs, and patched. Built from the crates.io tarball,
          # which ships its Cargo.lock, so the dependency set is the one
          # upstream tested.
          typdiff = pkgs.rustPlatform.buildRustPackage rec {
            pname = "typdiff";
            version = "0.1.4";
            src = pkgs.fetchCrate {
              inherit pname version;
              hash = "sha256-lb0xpYEameXDocIKGPs8ObiL9dhRAQXmEE1Z1Bl2LuM=";
            };
            cargoHash = "sha256-O/C3yIgHJrJXb6/8tr/KLfQzOu9YsfYPq71WHXjGUmY=";
            # Keeps labels on their elements, deleted blocks in order and
            # unnumbered, marks changed calls, and adds --no-preamble. See
            # PLAN.md for the defects the spike found.
            patches = [ ./nix/typdiff/0001-Keep-labels-and-numbering-intact-in-diff-output.patch ];
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

          # Official Typst release archives, exactly as the service downloads
          # them. The integration tests serve these from a local HTTP server,
          # so the download, hash check and unpacking run offline.
          typstTarget = {
            x86_64-linux = "x86_64-unknown-linux-musl";
            aarch64-linux = "aarch64-unknown-linux-musl";
          }.${system};
          typstReleaseHashes = {
            "0.15.1" = {
              x86_64-linux = "sha256-ptB30Kle7VouunFbLa4GvpVPYkzL+FdYoD84ne0zEYw=";
              aarch64-linux = "sha256-WqjXSj2QbmDqEqZqwvN/ju8bFMutcYKnReOToQwj3O4=";
            };
            "0.14.2" = {
              x86_64-linux = "sha256-pgRMutKpVN65IRZ+JX4SCsChayAznsARIRlP+dOUmW0=";
              aarch64-linux = "sha256-SRsQGqQKOn6oKj+KYjLKu05qfiM4EAguWsgS1D/c1Ho=";
            };
          };
          typstReleases = pkgs.linkFarm "typst-releases" (lib.mapAttrsToList
            (version: hashes: {
              name = "v${version}/typst-${typstTarget}.tar.xz";
              path = pkgs.fetchurl {
                url = "https://github.com/typst/typst/releases/download/v${version}/typst-${typstTarget}.tar.xz";
                hash = hashes.${system};
              };
            })
            typstReleaseHashes);

          thesis = pkgs.buildGoModule {
            pname = "thesis";
            version = "0.1.0";
            src = lib.cleanSource ./.;
            vendorHash = lib.fakeHash;
            subPackages = [ "cmd/thesis" ];
            env.CGO_ENABLED = "0";
            ldflags = [ "-s" "-w" ];
            # The tests need git, typst and the diff tools, and the release
            # archives above; `nix flake check` runs them through checks.
            doCheck = false;
            meta.mainProgram = "thesis";
          };
        in
        {
          packages = {
            default = thesis;
            inherit thesis typdiff typstReleases;
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
            THESIS_TEST_TYPST_RELEASES = typstReleases;
            THESIS_TEST_TYPST_TARGET = typstTarget;
          };
        });
}

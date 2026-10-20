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

          # diff-pdf only draws into an off-screen buffer, so its wxWidgets
          # needs no web view or OpenGL. The stock build pulls in WebKitGTK and
          # GStreamer, which made the image 1.05 GB.
          diff-pdf = pkgs.diff-pdf.override {
            wxwidgets_3_2 = pkgs.wxwidgets_3_2.override {
              withWebKit = false;
              withMesa = false;
            };
          };

          # External programs the service shells out to at run time. Typst is
          # absent on purpose: the service downloads the exact version each
          # commit pins (see .typst-review.toml in the brief).
          runtimeDeps = [
            pkgs.git
            diff-pdf
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

          goArgs = {
            pname = "thesis";
            version = "0.1.0";
            src = lib.cleanSource ./.;
            vendorHash = "sha256-z1h2erfw2wZ9y0QRVg/HL+W4956pAFIz6ROSQPGGC1Y=";
            env.CGO_ENABLED = "0";
          };

          thesis = pkgs.buildGoModule (goArgs // {
            subPackages = [ "cmd/thesis" ];
            ldflags = [ "-s" "-w" ];
            # The tests need git, the diff tools and the Typst archives;
            # checks.tests runs them.
            doCheck = false;
            meta.mainProgram = "thesis";
          });

          # Every package's tests, with the tools on PATH and the Typst
          # archives served locally, as in the dev shell.
          tests = pkgs.buildGoModule (goArgs // {
            pname = "thesis-tests";
            nativeCheckInputs = runtimeDeps ++ [ pkgs.bash ];
            THESIS_TEST_TYPST_RELEASES = typstReleases;
            THESIS_TEST_TYPST_TARGET = typstTarget;
            preCheck = ''
              export HOME=$TMPDIR
              patchShebangs scripts
            '';
            installPhase = "touch $out";
          });

          # The container's runtime. git is the minimal build: HTTPS fetches
          # and worktrees, without Perl, Python or the GUI tools.
          imageRuntime = [
            pkgs.gitMinimal
            diff-pdf
            pkgs.poppler-utils
            typdiff
          ];

          # Only what /etc must hold: the 99:100 user the container runs as,
          # so git and GTK can look up a home directory.
          imageEtc = pkgs.runCommand "thesis-etc" { } ''
            mkdir -p $out/etc
            printf 'root:x:0:0:root:/var/empty:/noshell\nthesis:x:99:100:thesis:/tmp:/noshell\n' >$out/etc/passwd
            printf 'root:x:0:\nusers:x:100:\n' >$out/etc/group
          '';

          # FROM-scratch image: the Nix closure of the binary and its tools,
          # nothing else. No shell, no package manager. Load it with
          # `nix build .#image && ./result | docker load`.
          image = pkgs.dockerTools.streamLayeredImage {
            name = "benmandrew/thesis";
            tag = "latest";
            contents = [
              (pkgs.buildEnv {
                name = "thesis-root";
                paths = [ thesis ] ++ imageRuntime;
                pathsToLink = [ "/bin" ];
              })
              imageEtc
            ];
            # Mount points for the read-only root, which Docker would
            # otherwise have to create.
            extraCommands = ''
              mkdir -p data etc/thesis
              mkdir -m 1777 tmp
            '';
            config = {
              Entrypoint = [ "/bin/thesis" ];
              Cmd = [ "serve" ];
              User = "99:100";
              WorkingDir = "/data";
              Env = [
                "PATH=/bin"
                "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
                "THESIS_CONFIG=/etc/thesis/config.toml"
              ];
              Volumes."/data" = { };
            };
          };
        in
        {
          packages = {
            default = thesis;
            inherit thesis typdiff typstReleases image;
          };

          checks = { inherit tests; };

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

# CLAUDE.md

A Go service that builds a PDF for every commit of one Typst thesis repository on Gitea and serves diffs between versions at `thesis.benmandrew.com`. It runs on an arm64 host. A separate Ansible repository deploys it; `CLAUDE.local.md`, if present, says where.

- Design: @Typst Review Server — Implementation Brief.md
- Progress and spike results: @PLAN.md

## Rules

- Every dependency comes from `flake.nix`. Work inside `nix develop`. Never install a tool any other way.
- Build with `CGO_ENABLED=0`, so the binary cross-compiles to arm64. Pick pure-Go libraries for that reason.
- Pass `--creation-timestamp` to every `typst compile`. The dev shell exports `SOURCE_DATE_EPOCH`, which hides a missing flag.
- Run nothing from the thesis repo except `typst compile`. Pass `git` arguments as arrays, never through a shell.
- Every route needs a passkey login except `/healthz` and `/hooks/push`. Check roles and note ownership on the server.

## Commands

- Tests: `nix develop -c go test ./...`, or `nix build .#checks.x86_64-linux.tests` for the sandboxed run CI uses.
- Image: `nix build .#image && ./result | docker load`. CI builds it per architecture and pushes with `.gitea/workflows/image.yml`.

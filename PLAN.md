# Plan

The design is in `Typst Review Server — Implementation Brief.md`. This file tracks progress and records what the spikes found.

## Stack

- **Go**, standard library first: `net/http` for the server, `html/template` for pages, `crypto/hmac` for webhooks, goroutines and `time.Ticker` for the job queue and timers.
- **SQLite** through `modernc.org/sqlite`, a pure-Go driver, so the binary builds with `CGO_ENABLED=0` and cross-compiles to arm64.
- **TOML** config through `github.com/BurntSushi/toml`.
- **Nix flake** for every dependency: the Go toolchain, the external programs the service runs (`git`, `diff-pdf`, `poppler-utils`, `typdiff`), and the container image.

## Spike results (2026-10-20, typdiff 0.1.4, Typst 0.15.1)

Fixture: a `main.typ` that includes two chapters, with inline and display maths, a labelled equation, a labelled table figure, citations, a `#let` binding, and a heading reference across files. The spike files are not kept; the fixture repo (step 3 below) rebuilds them.

**Reproducible PDFs.** Typst 0.15.1 accepts both `--creation-timestamp` and `SOURCE_DATE_EPOCH`. Three builds of the same tree gave the same SHA-256. The service passes `--creation-timestamp` explicitly, because the Nix dev shell exports `SOURCE_DATE_EPOCH=315532800` and would otherwise mask a missing flag in tests.

**typdiff is self-contained per file.** Each output starts with its own `#let diff-added` and `#let diff-deleted`. Styling cannot be changed by a flag, so the service strips that preamble and imports a shared `diff-markup.typ` instead.

**typdiff output fails to compile on real papers.** Five defects showed up:

1. A label moves to its own paragraph, so it attaches to whatever precedes it. A label after a changed display equation attaches to `#diff-added[...]`, and any `@ref` to it fails with `cannot reference styled`. This alone sends a typical thesis diff to the diff-pdf fallback.
2. A deleted display equation is emitted above the paragraph that preceded it, out of document order.
3. A deleted equation still takes an equation number, so every later number shifts by one.
4. A changed function call, such as `#figure(table(...))`, appears as the old call commented out with `//` followed by the new call unmarked. The reader sees no change.
5. A changed `#let` binding is handled the same way and is equally invisible.

What works: word-level prose diffs, citation changes (deleted `@key` is escaped), inline maths diffed inside the formula, added headings and paragraphs, and references across included files.

**Decision.** The fork route was taken. The flake applies `nix/typdiff/0001-Keep-labels-and-numbering-intact-in-diff-output.patch`, which fixes all five defects and adds a `--no-preamble` flag. The service writes its own `diff-markup.typ` and imports it.

## Steps

- [x] 1. Flake with dev shell and typdiff package
- [x] 2. Spike typdiff and Typst reproducibility
- [x] 3. Fixture repo generator (`scripts/make-fixture.sh`). Its acceptance tests live in `internal/service` and `internal/web`.
- [x] 4. Config loading and webhook verification
- [x] 5. SQLite schema, job queue and worker pool
- [x] 6. Mirror, worktrees and per-commit builds, with the Typst download and hash check
- [x] 7. Diff chain: typdiff, then diff-pdf, then a text diff
- [x] 8. Supervisor notes: the Source page, annotated PDFs with the three-level placement fallback, and carrying notes forward
- [x] 9. Maintenance timers
- [x] 10. Passkey login and enrolment, the UI, the stable URLs and the cache headers
- [x] 11. Image from the flake, and the Gitea Actions workflow. The arm64 image has not been built yet.
- [ ] 12. Deployment, tracked in the deployment repository.

## Implementation notes

**Image.** The image is a `dockerTools.streamLayeredImage`, and no Dockerfile or buildx is involved. Job containers on the Gitea runner have no Docker socket; they reach the host *nix-daemon* through the mounted `/nix`. Continuous integration (CI) builds `packages.x86_64-linux.image` and `packages.aarch64-linux.image`. It pushes each with skopeo as `sha-<short>-amd64` and `sha-<short>-arm64`, then joins them with `crane index append` as `sha-<short>` and tags that `latest`. The amd64 image is 752 MB. It was 1.05 GB until the flake turned off WebKit and Mesa in diff-pdf's wxWidgets. That variant is not in the binary cache, and building it took about 4 minutes on the x86 dev machine. The image runs as 99:100 with `PATH=/bin`, `SSL_CERT_FILE` pointing at the Nix CA bundle, and `THESIS_CONFIG=/etc/thesis/config.toml`. A smoke test ran it with a read-only root, `cap_drop ALL` and a tmpfs `/tmp`. The healthcheck passed, and git reached GitHub over HTTPS.

**Tests.** `nix build .#checks.x86_64-linux.tests` runs every package's tests in the Nix sandbox, in 1 min 47 s. The web tests drive the real handlers over Transport Layer Security (TLS) with a software passkey (ES256, "none" attestation). They cover enrolment, a reused link, login, cross-site request forgery (CSRF) tokens, Origin, rate limits, PDF URLs and cache headers, the diff queue's 503, note permissions, and annotated PDFs.

**Cross-site links.** A GET for a PDF that is not built yet starts the build only when `Sec-Fetch-Site` is absent, `same-origin` or `none`. A link from another site gets a confirmation button, which POSTs with the CSRF token. Moving URLs and full-SHA URLs behave the same way.

**Who can write notes.** The brief says supervisors create notes. The implementation lets every logged-in user create notes and edit or delete their own open ones. Only the author resolves. Restrict creation to supervisors if the author should not write notes.

**Rate limits.** Each limit is a *token bucket*, kept per client address and per user. Enrolment allows 10 a minute, login 20, and note writes 30.

**Config path.** The config lives at `/etc/thesis/config.toml`, a read-only directory mount.

## Deployment

The deployment repository tracks the role, its open manual steps and the measurements still to take. Every test so far ran on an x86 machine, and nothing has run on arm64 yet.

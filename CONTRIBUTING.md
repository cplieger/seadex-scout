# Contributing to seadex-scout

Notes on the architecture, local workflow, and conventions specific to this
repo. The org-wide `cplieger` defaults still apply; this file adds the
code-grounded detail you need to land a change without tripping over the
load-bearing patterns.

> This is a small, single-purpose self-hosted tool. Contributions are welcome,
> but the maintainer optimises for a small, auditable tool over breadth of
> features. Please open an issue to discuss anything larger than a bug fix
> before writing code.

## What seadex-scout is (and isn't)

It does one thing: compare a Sonarr/Radarr anime library against SeaDex and
report, per title, when SeaDex recommends a better release than the one on disk.
It is **report-only**: it never downloads, grabs, or touches a torrent client,
and it ships no notifier of its own (alerting is the observability stack's job;
the repo ships reference Loki/Mimir rules in [`deploy/`](deploy)). Auto-grab and
a rescan nudge are deliberately out of scope for v1. Keep that focus when
proposing changes: "surface an actionable finding" fits; "grab it for me" does
not.

## Architecture

seadex-scout is a single Go binary that runs one compare cycle on start and then
every `poll_interval` (or, when `poll_interval` is `off`, sits resident-idle
while an external scheduler triggers cycles via the `poll` subcommand), emitting
findings as JSON to stdout (slog, shipped to Loki). It has no HTTP surface. The only cross-cycle state is a single atomic JSON
file (library snapshot, cached ID map, AniList memo, finding dedupe).

`main.go` + `build.go` are the **composition root**: `main.go` installs logging,
handles the `health`/`report`/`poll`/`daemon` subcommands, loads/validates the
YAML config (writing a starter on first boot), and runs the daemon (built-in
interval or resident-idle), a one-shot report, or a single `poll` cycle;
`build.go` wires every component together. They contain no business logic;
everything testable lives under `internal/`, with dependencies flowing one
direction (leaves have no internal imports):

- `internal/config` — YAML config-file loading (with `${ENV}` expansion for
  secrets), clamping, and validation.
- `internal/seadex` — the releases.moe PocketBase client (paged entries with the
  torrents relation expanded), over `httpx`, bounded and polite.
- `internal/mapping` — the Fribb anime-lists loader (conditional GET + cache) and
  the local overrides overlay, indexed by AniList ID. `fribb.go` decodes the
  upstream JSON resiliently (per-record, tolerant of shape variance).
- `internal/anilist` — the AniList GraphQL fallback client with a header-adaptive
  throttle (spacing + `X-RateLimit` backoff + `Retry-After`).
- `internal/library` — the arrapi walk (Sonarr series + episodes, Radarr movies),
  arr-side tag include/exclude, per-item fingerprint and per-season groups, and
  the snapshot diff.
- `internal/release` — the pure classification engine: names/notes/metadata into
  a normalized `Release` (group, tracker kind, resolution, codec, dual-audio,
  remux-vs-encode). It imports no domain packages so both the SeaDex and library
  sides classify into one vocabulary.
- `internal/filter` — the operator's release filters (remux policy, resolution
  floor, tracker allowlist, dual-audio).
- `internal/match` — links a SeaDex entry to a library item (ID via the map, then
  the AniList title fallback) and reports mapping coverage.
- `internal/compare` — the group-centric comparison producing `Finding`s (aligned
  items emit nothing; the rest are warn/info findings, each with a dedupe key).
- `internal/audit` — the season-level report generator for report mode: a verdict
  per in-library match, rendered as Markdown + JSON + per-row slog.
- `internal/report` — the slog finding emitter with cross-cycle dedupe
  (observability is slog-only; no metrics).
- `internal/state` — the atomic JSON cache load/save (via `atomicfile`).
- `internal/scout` — the cycle orchestrator that wires the above into one cycle.

## Health and degradation semantics

Cycle health follows the **library ingest**: a failed arr walk (bad config,
unreachable arr) marks the container unhealthy, because a restart or config fix
could recover it. A SeaDex, mapping, or AniList failure is **degraded but
healthy** — a restart cannot fix an upstream outage, so the cycle logs a warning
and, for a SeaDex failure, preserves the existing findings rather than falsely
resolving them. The distroless `HEALTHCHECK` uses the `seadex-scout health`
file-marker subcommand; there is no HTTP health endpoint (Gatus watches the
container via the Komodo API instead).

## Development environment

You need Go; the exact version is pinned in [`go.mod`](go.mod).

```bash
git clone https://github.com/cplieger/seadex-scout
cd seadex-scout
GOTOOLCHAIN=auto go build ./...
```

seadex-scout is the first consumer of [`arrapi`](https://github.com/cplieger/arrapi)
and uses its `ResolveTagIDs` API plus `Series.TitleSlug` and the `WebURL`
deep-link helpers. These shipped in arrapi `v1.5.0` (`WebURL`; the others
earlier), which `go.mod` now requires — so the published image build (which uses
`go.mod`/`go.sum`) builds directly. The interim `go.work` replace to a local
arrapi checkout has been removed.

## Running checks

```bash
GOTOOLCHAIN=auto go build ./...
GOTOOLCHAIN=auto go vet ./...
GOTOOLCHAIN=auto go test -race -count=1 ./...
golangci-lint run ./...        # lint
golangci-lint fmt ./...        # apply gofumpt + gci formatting
```

A few house rules the linters enforce that are easy to trip on:

- **`sloglint` kv-only**: plain key/value pairs in `slog` calls, not attribute
  constructors.
- **Logs are UTC**: the `slogx` library (its `UTCTime` `ReplaceAttr`) forces every record's
  timestamp to UTC, so the image needs no `TZ` and embeds no `time/tzdata`.
- **`fieldalignment`**: order struct fields to minimise padding. The
  `fieldalignment -fix` tool reorders them for you but strips field comments, so
  reorder by hand when a struct's fields are documented.
- **Bounded everything**: outbound responses are size-capped before decode;
  untrusted upstream JSON (SeaDex, Fribb, AniList) is parsed defensively.
- **No new non-`cplieger` runtime dependencies** without discussion; the small,
  auditable supply chain is a feature (today the runtime deps are the `cplieger`
  shared libraries plus `go.yaml.in/yaml/v3` for the config file).

## Commits and pull requests

Branch from `main`, keep changes focused, and open a PR. Commit messages follow
[Conventional Commits](https://www.conventionalcommits.org/); git-cliff parses
them to build release notes and pick the version bump (`feat:` → minor, `fix:` /
`sec:` → patch/security, `feat!:` → major; `chore`, `ci`, `docs`, `test`, etc.
don't release). Write the subject as the changelog line a user would read. CI
must be green: the required `ci / validate` check builds the binary and
Dockerfile and runs vet/lint/race-tests/govulncheck. Releases are automated on
merge to `main`; contributors don't tag or publish manually.

## Conduct and security

By participating you agree to the org-wide
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security vulnerabilities through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.

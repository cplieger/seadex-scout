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

It has a tight scope: compare a Sonarr/Radarr anime library against SeaDex and
surface where they diverge. It does that three ways — a **monitoring daemon**
that logs a finding whenever SeaDex has a better release (for Loki alerting), an
on-demand **season-level report**, and an optional **Torznab feed** Sonarr/Radarr
grab from. The first two are **report-only**: they never download, grab, or touch
a torrent client, and the app ships no notifier of its own (alerting is the
observability stack's job; the repo ships reference Loki-ruler rules in
[`alerts.yaml`](alerts.yaml)). The feed is the one automation path, and even it
does not grab — it hands releases to the arrs so they grab through their own
engine; seadex-scout never pushes to a download client. Keep that boundary when
proposing changes: "surface an actionable finding" and "let the arrs act on
SeaDex's picks" fit; "push it to qBittorrent for me" does not.

## Architecture

seadex-scout is a single Go binary. The daemon runs one compare cycle on start
and then every `poll_interval` (or, when `poll_interval` is `off`, sits
resident-idle while an external scheduler triggers cycles via the `poll`
subcommand), emitting findings as JSON to stdout (slog, shipped to Loki), and —
when a Prowlarr Torznab URL is configured — also serves the Torznab feed in the
same process. It binds no HTTP port unless that feed is configured. The only
cross-cycle state is a single atomic JSON file (library snapshot, cached ID map,
AniList memo, finding dedupe).

`main.go` + `build.go` are the **composition root**: `main.go` installs logging,
handles the `health`/`report`/`poll`/`daemon` subcommands, loads/validates the
YAML config (writing a starter on first boot), and runs the daemon (built-in
interval or resident-idle, plus the Torznab feed when configured), a one-shot
report, or a single `poll` cycle; `build.go` wires every component together
(including the feed via `buildIndexer`/`startIndexer`). They contain no business
logic; everything testable lives under `internal/`, with dependencies flowing one
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
- `internal/filter` — the operator's release filters (remux policy, the
  AnimeBytes on/off toggle, dual-audio), split from tracker obtainability. These
  shape the report/alert engine only; the indexer feed applies none of them.
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
- `internal/indexer` — the Torznab feed server the daemon runs when a Prowlarr
  Torznab URL is configured. It proxies Prowlarr's Nyaa + AnimeBytes endpoints,
  filters the results to SeaDex's curation (matched by tracker id / info hash),
  and adds the download-volume-factor marker. It serves each tracker on its own
  path (`/nyaa`, `/ab`) or subdomain as well as combined, so an arr can gate a
  tracker's search-type use through that indexer's own flags. It depends only on
  `internal/seadex` (for the curation set) and an HTTP client — no arr, mapping,
  or scout wiring.

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

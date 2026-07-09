# seadex-scout design

Report-only watcher that compares a Sonarr/Radarr anime library against
[SeaDex](https://releases.moe) (the community-curated index of the best anime
releases) and emits a structured log line and a metric whenever SeaDex
recommends a better release than the one on disk. It never downloads, grabs, or
touches a torrent client; it tells you what to go get, and you decide.

> Status: design under review, not yet approved for implementation. Two
> adversarial reviews (2026-07) hardened the claims below; their consensus
> corrections are folded in.

## Motivation

SeaDex curates the best available release per anime (best encode, best remux,
per tracker), keyed by AniList ID. Keeping a library aligned with it by hand
means opening `releases.moe`, looking up each show, and eyeballing whether your
files match the recommendation. [`bbtufty/seadexarr`](https://github.com/bbtufty/seadexarr)
automates the lookup but has two gaps that matter here (both verified against
its README and config sample):

- Its only notifier is Discord (`discord_url`). This stack alerts from Loki and
  Mimir through Grafana, not a webhook.
- Its filters are `public_only`, `prefer_dual_audio`, `want_best`,
  `ignore_tags`, and `trackers`. None of them keep encodes and drop remuxes. For
  a storage-and-bandwidth-conscious library that prefers a good x265 encode over
  a 40 GB remux, that distinction is the whole point.

seadex-scout is a small Go service that closes both gaps and nothing more.

**Report-only is a deliberate stance, not a missing feature.** seadexarr's value
proposition is the opposite: it grabs, hands-free, into qBittorrent. This design
keeps a human in the loop on purpose. SeaDex recommendations are curation
judgments that interact with storage budget, tracker access, and per-show taste;
for this library, a nudge the operator acts on beats an automated grab. The cost
is honest: a finding is only useful if acting on it is easy, so the report
carries the exact SeaDex and tracker links (see the URL handling in the
comparison section). Auto-grab and a rescan nudge are explicitly out of v1 (open
question 2).

## Goals and non-goals

Goals:

- Match the Sonarr/Radarr library to SeaDex entries by AniList ID, with a title
  fallback for the entries that do not map (series and movies alike).
- Classify every candidate SeaDex release (remux vs encode, resolution, group,
  tracker, dual-audio) and filter it by the operator's preferences.
- Report, per matched item, when SeaDex recommends a release the library does
  not have, as a structured slog event and a Prometheus metric.
- Be cheap to run and observable: cache the library walk and the ID map, and
  keep AniList traffic near zero.

Non-goals (deliberate, not deferred):

- No torrent grabbing, no qBittorrent or download-client integration, no
  auto-import. Report-only.
- No built-in Discord or other notifier. Alerting is the observability stack's
  job (Loki/Mimir alert rules, Grafana, Alertmanager); seadex-scout ships the
  rules as deployment artifacts (see observability).
- No media CRUD in Sonarr/Radarr. Read-only.
- No quality-profile filtering. Tags do the include/exclude job better (operator
  decision), so profiles are out.

## Data sources

### SeaDex (`releases.moe`, PocketBase)

- `GET /api/collections/entries/records?expand=trs&page=N&perPage=500` paginates
  all entries (2768 observed 2026-07; treat as a live count, not a constant).
  Each entry: `alID` (AniList ID, int), `notes`, `comparison`, `incomplete`
  (bool), `theoreticalBest`, and `trs` (expanded torrents).
- Each torrent: `releaseGroup`, `tracker` (`Nyaa`, `AB`, `BeyondHD`, ...),
  `isBest` (bool), `dualAudio` (bool), `files[]` (`{name, length}`), `infoHash`,
  `url`, `tags`, `updated`.
- The full set is ~6 pages at `perPage=500`. v1 pulls all entries each cycle and
  filters in memory; no partial SeaDex-freshness diffing (premature at this size,
  and SeaDex publishes no API contract to rely on). A small inter-page delay
  (default 2s, matching seadexarr's politeness) and a descriptive `User-Agent`
  keep it a good citizen against a Cloudflare-fronted community service.
  Responses are size-bounded before decode.

### Fribb anime-lists (the ID bridge)

SeaDex keys everything on AniList IDs. Sonarr keys on TVDB, Radarr on
TMDB/IMDB. [`Fribb/anime-lists`](https://github.com/Fribb/anime-lists)
(`anime-list-mini.json` plus `indices/anilist_index.json`) maps `anilist_id` to
`type` (`TV`/`MOVIE`/`OVA`/`ONA`/`SPECIAL`), `tvdb_id`,
`themoviedb_id.{tv | movie[]}`, `imdb_id[]`, and `season.{tvdb,tmdb}` /
`episode_offset.{tvdb,tmdb}`. It is a single static file, downloaded once and
refreshed on a slow cadence (24h default) with a conditional GET
(ETag/If-Modified-Since) so an unchanged multi-MB file is not re-fetched.

Three nuances the mapping forces us to respect:

- TVDB and TMDB reuse the same integer across TV and movies, so the `type` field
  decides which arr and which ID field to use (`themoviedb_id.movie` is an
  array; `.tv` is scalar).
- An AniList entry is a season or cour; a Sonarr series is the whole TVDB show.
  The map ships `season` and `episode_offset`; seadex-scout consumes them to
  resolve the entry to the right series (and season, where scoping is enabled).
- Fribb merges its sources on the AniDB ID and cannot take corrections directly.
  Coverage therefore depends on the upstream AniDB-to-AniList link, which lags
  for brand-new or niche entries. seadex-scout loads a local
  `mapping_overrides.json` (keyed by `alID`) ahead of Fribb so the operator can
  pin the misses, and it emits a coverage metric (below) so misses are visible
  rather than silent. An earlier point-in-time measurement on the live library
  (about 100% of series, 95% of movies) is treated as an observation to track,
  not a guarantee of the data source.

### AniList GraphQL (fallback + enrichment only)

`https://graphql.anilist.co`. The documented steady limit is 90 requests/minute,
but the API is currently in a documented degraded state at 30/minute, so the
throttle is config-driven (`ANILIST_RATE`, default 30), reads `X-RateLimit-Limit`
/ `X-RateLimit-Remaining` to back off before a 429, and honors `Retry-After`
(httpx already parses it). Used only when the Fribb map plus overrides miss:
fetch the entry's titles and `format`, then attempt a normalized title-plus-year
match against the library. The fallback covers both Sonarr series and Radarr
movies (an unmapped series must not vanish silently). Matching is conservative:
exact normalized title + year, single candidate required, otherwise logged as a
manual mapping miss rather than fuzzy-guessed. Mapped items never hit AniList,
so steady-state AniList traffic is near zero.

### Sonarr / Radarr (via `arrapi`)

The [`cplieger/arrapi`](https://github.com/cplieger/arrapi) v1.1.0 client:
`GetSeries`/`GetEpisodes` (Sonarr) and `GetMovies` (Radarr) give `TvdbID`,
`TmdbID`, `ImdbID`, `Tags`, `Title`, `Year`, and per-file `ReleaseGroup`,
`SceneName`/`RelativePath`, and `MediaInfo`. Tag include/exclude uses
`TagIDs`/`HasAnyTag`. seadex-scout declares its own narrow `ArrClient`
interface that `*arrapi.Sonarr`/`*arrapi.Radarr` satisfy structurally
(consumer-side interface placement).

## Pipeline

One poll cycle, run on start and then every `POLL_INTERVAL` (12h default):

1. **Ingest library.** Walk Sonarr series (plus episode files) and Radarr movies
   through `arrapi`. Apply tag include/exclude. Build a snapshot: per item, its
   external IDs, tags, and current release fingerprint (group, resolution,
   remux/encode, dual-audio) from the file names and `MediaInfo`. Diff against
   the cached snapshot to know what changed on the arr side.
2. **Refresh mapping.** Load `mapping_overrides.json`, then the cached Fribb map
   (conditional re-download if older than `MAPPING_REFRESH`). Index by
   `anilist_id`.
3. **Pull SeaDex.** Page through all `entries` with `expand=trs` (polite delay,
   User-Agent, bounded reads).
4. **Match.** For each SeaDex entry: resolve `alID` via overrides then Fribb to
   external IDs + `type` (+ `season`/`episode_offset`), and find the library item
   (Sonarr by `TvdbID`, Radarr by `TmdbID`/`ImdbID`). On a map miss, fall back to
   an AniList title lookup and a conservative title+year match (series and
   movies). No match means the anime is not in the library; skip it and count it
   toward the coverage metric.
5. **Classify and filter candidates.** Classify each SeaDex torrent (release
   engine below) and drop the ones the operator's filters exclude (remux policy,
   resolution floor, tracker allowlist, dual-audio). What survives is the set of
   recommended releases the operator could actually use.
6. **Compare.** Fingerprint the library's current release for the matched item
   and compare it against the surviving recommended set (comparison rule below).
   Aligned items emit nothing; the rest are findings.
7. **Report and dedupe.** Emit a new or changed finding as a structured slog
   event and set the metric; suppress a finding already alerted (same item, same
   recommendation, same library state) via the dedupe state. When a prior finding
   becomes aligned, drop it from the gauge and log one `info` resolution. Persist
   snapshot, map cache, AniList memo, and dedupe state atomically.

## Release classification engine

The core novel piece. Input is a SeaDex torrent (`releaseGroup`, `tracker`,
`dualAudio`, `files[].name`) plus the entry `notes`; output is a normalized
`Release`:

- `Group` from `releaseGroup`; `Tracker` classified public (`Nyaa`) vs private
  (`AB`, `BeyondHD`) so the operator can filter to releases they can obtain.
- `Resolution` (`2160p`/`1080p`/`720p`), `Codec` (`x265`/`HEVC`/`x264`/`AVC`),
  and `DualAudio`, parsed from the names and the field.
- `Kind` (`remux` | `encode` | `unknown`), classified from the release name and
  the entry `notes` only (no size or bitrate inference):
  1. **Remux** when a name or note carries an explicit marker (`Remux`,
     `BDRemux`, `BD Remux`, `REMUX`), or the group is pinned as remux in the
     operator overrides (for example "CRUCiBLE is remux"). On SeaDex a remux is
     stated in the release name or the notes, which is what makes name-and-notes
     parsing reliable here.
  2. **Encode** when the name carries an encoder/transcode marker (`x265`,
     `x264`, `HEVC`, `AVC`, a CRF/bitrate tag, or a group suffix like `-koala`)
     and no remux marker is present.
  3. Otherwise `unknown`, which is surfaced and never silently dropped, with a
     `classification_reason` recorded.

The same name-based fingerprinting runs on the library file (arrapi
`ReleaseGroup` + `SceneName`/`RelativePath` + `MediaInfo`) so both sides compare
in the same vocabulary.

Filters (all optional, config-driven):

- `FILTER_ALLOW_REMUX` (default false): when false, releases classified `remux`
  never count as a recommendation. `unknown` is never auto-dropped.
- `FILTER_MIN_RESOLUTION`, `FILTER_TRACKERS` (allowlist),
  `FILTER_REQUIRE_DUAL_AUDIO`.
- Tag include/exclude on the arr side (`INCLUDE_TAGS`, `EXCLUDE_TAGS`).

## Comparison rule

SeaDex is fundamentally a release-group recommendation, so the comparison is
group-centric, defaulting to **series-level group membership**: after filtering,
the recommended set is the groups SeaDex marks `isBest` that pass the filters,
and an item is aligned if a recommended group is present on the matched series
(or the movie). This sidesteps most multi-cour, absolute-numbering, and
batch-vs-per-season breakage. Season-scoping (using Fribb's `season`) is
available behind a flag for operators who want per-season precision.

Two refinements keep findings honest:

- Group match alone can miss a same-group quality bump (SeaDex swapped its best
  release within the same group). The dedupe key includes the SeaDex `infoHash`,
  which changes when the release does, so a same-group upgrade still surfaces.
- A season whose episodes span multiple release groups is reported as
  `comparison_status="mixed_group_manual"` (an `info`-level manual-review nudge),
  not a false "better release" finding.

Edge cases:

- `incomplete` entries and `theoreticalBest`-only entries report at `info`
  severity; there is nothing better to actually grab.
- When every `isBest` release sits on a private tracker excluded by the
  allowlist, the finding is suppressed or downgraded (configurable).
- Private-tracker URLs from SeaDex are relative (for example AnimeBytes returns
  `/torrents.php?id=..`). The report prefixes them with the tracker's base URL so
  the link is usable, rather than emitting a broken path.

## State and caching

A single JSON state file, written atomically with
[`cplieger/atomicfile`](https://github.com/cplieger/atomicfile):

- `library`: last snapshot (per item: arr id, external IDs, tags, current
  fingerprint) for diffing.
- `mapping`: the cached Fribb file plus its ETag/timestamp.
- `anilist`: memoized title/format lookups by `alID` (they do not change).
- `findings`: dedupe state keyed by `alID` + recommended-group-set +
  library-current-state + SeaDex `infoHash`, with last-alerted time.

SeaDex itself is pulled fresh each cycle (no freshness cache in v1); the cache
covers the expensive and stable parts (library diff, the ID map, AniList memos).

## Configuration

Environment variables (secrets via age-encrypted `.env`, the rest via
`servers/<server>.env` conventions):

| Variable | Default | Purpose |
| --- | --- | --- |
| `SONARR_URL`, `SONARR_API_KEY` | none | Sonarr instance (required) |
| `RADARR_URL`, `RADARR_API_KEY` | none | Radarr instance (required) |
| `POLL_INTERVAL` | `12h` | Cycle cadence; also runs once on start |
| `SEADEX_BASE_URL` | `https://releases.moe` | SeaDex API base |
| `SEADEX_PAGE_DELAY` | `2s` | Politeness delay between SeaDex pages |
| `MAPPING_URL` | Fribb raw JSON | Anime-list map source |
| `MAPPING_REFRESH` | `24h` | Conditional re-download cadence for the map |
| `MAPPING_OVERRIDES` | `/data/overrides.json` | Local `alID` map overrides |
| `ANILIST_URL` | `https://graphql.anilist.co` | Title/format fallback |
| `ANILIST_RATE` | `30` | Max AniList requests/min (adaptive off headers) |
| `FILTER_ALLOW_REMUX` | `false` | Count remuxes as recommendations |
| `FILTER_MIN_RESOLUTION` | `1080p` | Minimum recommended resolution |
| `FILTER_TRACKERS` | all | Allowlist of trackers the operator can use |
| `INCLUDE_TAGS`, `EXCLUDE_TAGS` | none | Arr-side tag include/exclude |
| `STATE_PATH` | `/data/state.json` | Cache/state file |
| `METRICS_ADDR` | `:9090` | Prometheus + health listener (LAN-only) |
| `LOG_LEVEL`, `LOG_FORMAT` | `info`, `json` | slog config |

## Observability

- **slog to Loki.** JSON handler to stdout, collected by the per-server Alloy
  agent. A finding is one line at `warn`: `msg="better release available"` with
  `title`, `alID`, `arr`, `current_group`, `recommended_group`, `tracker`,
  `resolution`, `kind`, `classification_reason`, and a usable `release_url`.
  Informational cases (`incomplete`, private-only, `mixed_group_manual`,
  resolutions) log at `info`.
- **Metrics to Mimir.** A `/metrics` endpoint scraped by Alloy:
  `seadex_scout_better_release{arr,tracker}` (gauge of current findings),
  `seadex_scout_unmapped_entries{arr}` and `seadex_scout_mapping_hits{arr}`
  (coverage), `seadex_scout_cycle_duration_seconds` (histogram),
  `seadex_scout_last_success_timestamp_seconds`, and counters for AniList calls
  and rate-limit waits. Labels stay low-cardinality (no title/group labels).
- **Alert artifacts.** The repo ships one Mimir/Prometheus alert expression (on
  the gauge) and one Loki alert query (on the log line) as deployment artifacts,
  so the app is not notifier-less in practice. No notifier in the app itself.
- **Health.** `cplieger/health` exposes `/healthz` on the same listener,
  reporting Sonarr/Radarr reachability (arrapi `Ping`) and last-cycle success.

## Deployment

Standalone repo `cplieger/seadex-scout` (a source repo, like `subflux`), built
to a distroless/nonroot image, referenced from
`homelab/apps/seadex-scout/compose.yaml` by pinned digest. It extends a
`rootless-strict` template from `base.yaml`, takes a small `mem_limit`, mounts a
named volume for `/data`, and gets its secrets from an age-encrypted `.env`. A
Gatus monitor and a provisioned Grafana dashboard round it out. CI, release
(git-cliff), and Renovate follow the standard `cplieger/ci` pipeline. Linking
`arrapi` (GPL-3.0) makes seadex-scout GPL-3.0, which is the intended homelab
license.

## Shared libraries used

- `arrapi` (Sonarr/Radarr client). seadex-scout is its first consumer.
- `httpx` (resilient outbound HTTP for SeaDex, AniList, and the map fetch: retry,
  transient classification, `Retry-After`, bounded reads, conditional GET). The
  AniList client wraps it with a header-adaptive throttle.
- `atomicfile` (crash-safe state writes).
- `health` and `metrics`, served with `webhttp`.

SSRF hardening (`cplieger/ssrf`) is considered and skipped: every outbound host
is fixed config (SeaDex, AniList, Fribb) or the operator's own arr URLs, none of
them per-request user input, so there is no SSRF surface. arrapi already
validates its base URL and refuses cross-host redirects; seadex-scout validates
the public endpoints at startup (https-only, defaulted hosts) as cheap
config hygiene.

## Go layout

```text
seadex-scout/
  main.go                # flags/env -> config -> scout loop -> signals
  internal/
    config/              # env parsing + validation
    seadex/              # releases.moe client (entries + torrents)
    mapping/             # Fribb loader + overrides + alID index
    anilist/             # header-adaptive GraphQL fallback client
    library/             # arrapi walk, snapshot, diff
    release/             # name/notes/size parsing + classification
    filter/              # remux/resolution/tracker/tag filters
    match/               # SeaDex entry <-> library item
    compare/             # current vs recommended -> findings
    report/              # slog events + metrics
    state/               # atomicfile cache + dedupe
    scout/               # cycle orchestrator
```

## Security

- API keys arrive via env from an age-encrypted `.env`; never logged (arrapi
  keeps the key out of errors and does not echo `X-Api-Key`).
- The only listener is `/metrics` + `/healthz`, bound LAN-only and
  unauthenticated. It exposes finding counts and health, no secrets, consistent
  with the other homelab exporters. Flagged because it is network-exposed.
- Outbound requests go only to fixed known hosts plus the operator's arr URLs,
  validated at startup.

## Open questions

1. **Match direction at scale.** Iterating SeaDex entries and probing the library
   index is the default. For a small library, iterating the library and probing
   SeaDex by `alID` is cheaper; the snapshot supports either. Confirm the default.
2. **Rescan hook.** arrapi exposes `RescanSeries`/`RescanMovie`. Out of scope for
   report-only v1; a candidate flag later if the operator wants seadex-scout to
   nudge the arr after a manual grab.
3. **Season-scoping default.** v1 defaults to series-level group membership
   (fewer false positives) with per-season scoping behind a flag. Confirm that is
   the right default versus per-season by default.
4. **Multi-instance arr.** The homelab runs one Sonarr and one Radarr; the design
   assumes one of each. Multi-instance is a config-shape change, not a redesign.

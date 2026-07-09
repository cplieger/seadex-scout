# seadex-scout

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/seadex-scout/badges/size.json)](https://github.com/cplieger/seadex-scout/pkgs/container/seadex-scout)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/seadex-scout/badges/coverage.json)](https://github.com/cplieger/seadex-scout/actions/workflows/coverage.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/seadex-scout/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/seadex-scout)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/seadex-scout/releases)

A report-only watcher that compares your Sonarr/Radarr anime library against
[SeaDex](https://releases.moe) (the community-curated index of the best anime
releases) and tells you, per title, when SeaDex recommends a better release than
the one on disk. It emits a structured log line for each finding (slog to Loki).
It never downloads, grabs, or touches a torrent client: it tells you what to go
get, and you decide.

## The problem

Keeping an anime library aligned with SeaDex by hand means opening
`releases.moe`, looking up each show, and eyeballing whether your files match the
recommendation. [`seadexarr`](https://github.com/bbtufty/seadexarr) automates the
lookup but has two gaps that matter for a storage- and bandwidth-conscious
library:

- Its only notifier is Discord. This stack alerts from Loki through Grafana, not
  a webhook.
- Its filters cannot keep encodes and drop remuxes. For a library that prefers a
  good x265 encode over a 40 GB remux, that distinction is the whole point.

seadex-scout closes both gaps and nothing more.

## What it does

Once on start and then every `poll_interval`, seadex-scout runs one cycle:

1. Walks the Sonarr/Radarr anime library (with arr-side tag include/exclude) and
   fingerprints each item's current release (group, resolution, codec,
   remux-vs-encode, dual-audio).
2. Matches each SeaDex entry to a library item by **AniList ID** through the
   [Fribb anime-lists](https://github.com/Fribb/anime-lists) ID bridge, with an
   **AniList title fallback** for the entries that do not map.
3. Classifies and filters SeaDex's recommended releases by your preferences
   (remux policy, resolution floor, tracker allowlist, dual-audio).
4. Compares the surviving recommendation against what you have and, when SeaDex
   has something better, emits a `warn` log line.

It is cheap to run and observable: it caches the library walk and the ID map,
keeps AniList traffic near zero, and logs mapping coverage each cycle so misses
are visible rather than silent.

## Run modes

The `mode` setting (or a subcommand) picks one of two modes:

- **daemon** (default): the poll loop above, flagging better releases as findings
  on the log (slog to Loki). It runs a cycle on start and every `poll_interval`,
  or sits resident-idle when `poll_interval` is `off` (see Scheduling below).
- **report**: a one-shot, read-only audit. It scans the whole library once,
  writes a SeaDex-alignment report, and exits. Trigger it as the container
  command (`report`), by setting `mode: report` in the config, or, while the
  daemon runs, via `docker exec`.

### Scheduling

The daemon follows the fleet's `*_INTERVAL` shape:

- **Built-in** (default): `poll_interval` is a Go duration (`12h`, `6h`, `30m`); a
  cycle runs on start, then every interval.
- **External / resident-idle**: set `poll_interval: off` (or `disabled` / `0`).
  There is no internal timer; the container idles healthy and an external
  scheduler drives each cycle via the `poll` subcommand — which runs one cycle,
  updates the health marker, and exits `0`/`1`. With
  [Ofelia](https://github.com/mcuadros/ofelia), label the service:

  ```yaml
      labels:
        ofelia.enabled: "true"
        ofelia.job-exec.seadex-poll.schedule: "@every 12h"
        ofelia.job-exec.seadex-poll.command: "/seadex-scout poll"
  ```

  Any scheduler works — `docker exec seadex-scout /seadex-scout poll` is the whole
  contract. This mirrors github-scout, docker-rsync-scheduler, and the other
  fleet schedulers.

### The report

The report answers, for every anime with a SeaDex match: which release you have,
and whether it is SeaDex's best, a listed alt, or neither. It is season-level:
each SeaDex entry (one AniList ID = one cour, movie, or special) is scoped to its
TVDB season through the Fribb mapping and compared against that season's on-disk
groups. Each row gets a verdict:

- `have_best`: you have a release SeaDex marks best.
- `have_alt`: you have a listed alt; SeaDex marks a different release best.
- `have_unlisted`: you have a release SeaDex does not list.
- `no_file`: the mapped season or movie has no file on disk.
- `unverified`: matched to a series but not resolvable to a season (a likely
  match, no release validation).

Every row carries three kinds of link: the Sonarr/Radarr deep-link to the item
(Sonarr via its title slug, Radarr via its TMDB id), the SeaDex entry
(`releases.moe/{alID}`), and the indexer link for each best release. The report
is written three ways, so it is both human- and machine-readable: a Markdown file
grouped by verdict (at `report.path`), a JSON file alongside it (`.json`), and one
`report item` slog line per anime (queryable in Loki like the daemon findings).

While the daemon runs, produce a fresh report without stopping it by running the
`report` subcommand in the container:

```sh
docker exec seadex-scout /seadex-scout report
```

The `.md` and `.json` land next to `report.path` on the `/config` volume, where
you read them directly. A report never writes the state cache, so it is safe to
run alongside a daemon cycle. To produce reports on a schedule, use the same
Ofelia `job-exec` pattern as above with `/seadex-scout report`.

## How matching works

SeaDex keys everything on AniList IDs; Sonarr keys on TVDB, Radarr on TMDB/IMDb.
seadex-scout bridges them:

- **ID mapping.** The Fribb `anime-list-mini.json` dataset maps `anilist_id` to
  `type` (TV vs movie), `tvdb_id`, `themoviedb_id`, and `imdb_id`. It is fetched
  with a conditional GET on a slow cadence and cached, so an unchanged multi-MB
  file is never re-downloaded. The `type` decides which arr and which ID field
  to use.
- **Overrides.** A local `mapping_overrides.json` (a JSON array of records keyed
  by `anilist_id`) is applied ahead of Fribb, so you can pin the entries Fribb
  misses.
- **Title fallback.** When an entry maps through neither, seadex-scout fetches
  its titles and format from AniList and attempts a conservative normalized
  title-plus-year match against the library (exact match, single candidate
  required; ambiguous matches are skipped, not guessed). Mapped items never hit
  AniList, so steady-state AniList traffic is near zero.

## Release classification and filters

Each SeaDex release and each library file is classified into one vocabulary:
release group, tracker (public like Nyaa vs private like AnimeBytes),
resolution, codec (x265/x264), dual-audio, and **kind** (`remux` / `encode` /
`unknown`). The remux-vs-encode decision is name- and notes-based, never a size
or bitrate inference; an unclassifiable release is `unknown` and is never
silently dropped.

The comparison is **group-centric**: an item is aligned when a recommended
release group is already present on it. This sidesteps most multi-cour and
batch-vs-per-season breakage. Filters (all optional):

- `filters.allow_remux` (default false): when false, releases classified `remux`
  never count as a recommendation. `unknown` is never auto-dropped.
- `filters.min_resolution` and `filters.require_dual_audio` gate quality.
- `filters.trackers` is a **preferred** allowlist, not a hard filter. A better
  release that exists only on a tracker you did not list is still flagged (tagged
  `unavailable_on_selected_trackers`) so you know it exists, unless you set
  `filters.notify_unavailable_tracker: false`, which keeps it informational
  (`private_only`) instead.
- `filters.include_specials` (default on): include OVA/ONA/special entries; turn
  it off to drop them from findings and the report.
- `tags.include` / `tags.exclude` (arr-side).

## Configuration

All configuration lives in one YAML file, `/config/config.yaml` (override the
path with `CONFIG_PATH`). On first boot with no config, seadex-scout writes a
commented starter there and exits with a warning; edit it and restart. The full
annotated template is [`config.example.yaml`](config.example.yaml).

Any string value may reference `SONARR_*`, `RADARR_*`, or `SEADEX_SCOUT_*`
environment variables with `${VAR}`, so secrets can live in an `.env` or Docker
secret instead of the file. API keys are never logged (only whether each is set).

```yaml
sonarr:
  enabled: true
  url: "http://sonarr:8989"       # internal URL seadex-scout queries
  api_key: "${SONARR_API_KEY}"    # or paste it directly
  public_url: ""                  # browser URL for report deep-links; falls back to url
radarr:
  enabled: false
  url: "http://radarr:7878"
  api_key: ""

mode: "daemon"                    # daemon | report
poll_interval: "12h"              # a cycle also runs once on start
season_scoping: false             # compare per-season instead of series-level

filters:
  allow_remux: false
  min_resolution: "1080p"         # "" disables the floor
  require_dual_audio: false
  trackers: []                    # preferred indexers, e.g. ["animebytes", "nyaa"]; [] = all
  remux_groups: []
  notify_unavailable_tracker: true
  include_specials: true

tags:
  include: []                     # only scan arr items with these tags; [] = all
  exclude: []

report:
  path: "/config/report.md"       # a .json is written alongside

seadex:
  base_url: "https://releases.moe"
  page_delay: "2s"

mapping:
  url: "https://raw.githubusercontent.com/Fribb/anime-lists/master/anime-list-mini.json"
  refresh: "24h"
  overrides_path: "/config/overrides.json"

anilist:
  url: "https://graphql.anilist.co"
  rate: 30                        # max requests/minute

state_path: "/config/state.json"

log:
  level: "info"                   # debug | info | warn | error
  format: "json"                  # json | text
```

At least one arr must be `enabled` with a `url` + `api_key`; an enabled arr
missing either is a configuration error. `public_url` is the browser-facing base
used only for the report's deep-links (leave empty to reuse `url`), so an
internal Docker hostname in `url` still yields working links.

## Observability

Observability is slog-only: there is no metrics endpoint and no HTTP surface.

- **slog to Loki.** A JSON handler writes to stdout; Alloy (or any collector)
  ships it to Loki. A finding is one line at `warn` (`msg="better release
  available"`, or `"...not on your selected trackers"` for
  `unavailable_on_selected_trackers`) with `title`, `al_id`, `arr`,
  `current_group`, `recommended_group`, `tracker`, `resolution`, `kind`,
  `classification_reason`, and a usable `release_url`. Informational cases
  (`incomplete`, `theoretical_best`, `private_only`, `mixed_group_manual`) log at
  `info`. Each cycle closes with a `cycle complete` line carrying the counts,
  mapping coverage, and AniList usage; report mode emits one `report item` line
  per anime.
- **Alert rules.** seadex-scout ships no notifier of its own. Reference Loki
  log-alert rules are in [`deploy/loki-rules.yaml`](deploy/loki-rules.yaml) (alert
  on the finding line, or on the absence of a recent `cycle complete` line for
  staleness). Wire them into your log-alerting stack.
- **Health.** The distroless image's Docker `HEALTHCHECK` uses the
  `seadex-scout health` subcommand (a `/tmp/.healthy` file marker), so no shell or
  port is needed; the marker reflects the last cycle's library-ingest outcome.

## Deployment

seadex-scout ships as a distroless, non-root, multi-arch (amd64 + arm64) image
at `ghcr.io/cplieger/seadex-scout`. The [`compose.yaml`](compose.yaml) at the
repo root is a minimal example. For a hardened deployment, layer on:

```yaml
    read_only: true
    cap_drop: ["ALL"]
    security_opt: ["no-new-privileges:true"]
    tmpfs: ["/tmp:size=1m,mode=1777,noexec,nosuid,nodev"]  # backs the health marker
```

The `/config` volume must be writable by the container user (set `user:` to match
the host owner of the mounted directory); it holds `config.yaml`, the state
cache, and the report output. There is no network port to expose:
observability is slog-only and health is the file-marker `health` subcommand, so
the container has no HTTP surface.

## Report-only, on purpose

seadexarr's value proposition is the opposite of this one: it grabs, hands-free,
into a download client. seadex-scout keeps a human in the loop deliberately.
SeaDex recommendations are curation judgments that interact with storage budget,
tracker access, and per-show taste; a nudge you act on beats an automated grab.
Every finding carries the exact SeaDex and tracker links so acting on it is easy.
Auto-grab and a rescan nudge are out of scope for v1.

## License

GPL-3.0. Linking [`arrapi`](https://github.com/cplieger/arrapi) (GPL-3.0) makes
seadex-scout GPL-3.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

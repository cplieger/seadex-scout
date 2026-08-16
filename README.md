# seadex-scout

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/seadex-scout/badges/size.json)](https://github.com/cplieger/seadex-scout/pkgs/container/seadex-scout)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/seadex-scout/badges/coverage.json)](https://github.com/cplieger/seadex-scout/actions/workflows/coverage.yml)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13869/badge)](https://www.bestpractices.dev/projects/13869)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/seadex-scout/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/seadex-scout)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/seadex-scout/releases)

A report-only watcher that compares your Sonarr/Radarr anime library against
[SeaDex](https://releases.moe) (the community-curated index of the best anime
releases) and tells you, per title, when SeaDex recommends a better release than
the one on disk. It never downloads, grabs, or touches a torrent client: it tells
you what to go get, and you decide.

One image and one config file give you three things:

1. **Findings on the log** (always on): the daemon compares your library to
   SeaDex and logs a `warn` line when a better release exists than the one on
   disk. You turn those lines into Loki/Grafana alerts (see
   [Alerting](#alerting)); the app ships no notifier of its own.
2. **An on-demand report**: a season-by-season audit of how your whole library
   lines up with SeaDex, written as Markdown and JSON. See
   [The report](#the-report).
3. **An optional [Torznab feed](#indexer-torznab-feed)**: publishes SeaDex's picks
   for Sonarr/Radarr to grab through their own engine. It stays off until you
   configure it, and it is the only automation path: seadex-scout itself still
   never grabs, the arrs do.

## The problem

To keep an anime library aligned with SeaDex by hand, you open `releases.moe`,
look up each show, and compare your files against the recommendation.
[`seadexarr`](https://github.com/bbtufty/seadexarr) automates the lookup, but two
gaps matter for a storage- and bandwidth-conscious library:

- Its only notifier is Discord, so it cannot alert through Loki and Grafana.
- Its filters cannot keep encodes and drop remuxes. For a library that prefers a
  good x265 encode over a 40 GB remux, that distinction is the whole point.

seadex-scout closes both gaps and nothing more.

## What it does

On start and then every `poll_interval`, seadex-scout runs one cycle:

1. It walks the Sonarr/Radarr anime library (with arr-side tag include/exclude)
   and fingerprints each item's current release: group, resolution, codec,
   remux-vs-encode, and dual-audio.
2. It matches each SeaDex entry to a library item by **AniList ID** through the
   [Fribb anime-lists](https://github.com/Fribb/anime-lists) ID bridge, with an
   **AniList title fallback** for the entries that do not map.
3. It filters SeaDex's recommended releases by your preferences (remux policy,
   AnimeBytes on or off, dual-audio).
4. It compares the surviving recommendation against what you have and emits a
   `warn` log line when SeaDex has something better.

When the [Torznab feed](#indexer-torznab-feed) is configured, the same cycle
rebuilds it from that one SeaDex fetch, so a finding and what the arrs can grab
from the feed always reflect the same refresh.

## Run modes

The `mode` setting (or a subcommand) picks the run mode:

- **daemon** (default): the poll loop above, flagging better releases as findings
  on the log. When a Prowlarr Torznab URL is configured, the same process also
  serves the [Torznab feed](#indexer-torznab-feed), both features in one
  container.
- **report**: a one-shot, read-only audit. It scans the whole library once, writes
  a SeaDex-alignment report, and exits. Run it as the container command
  (`report`), set `mode: report` in the config, or use `docker exec` while the
  daemon runs.

### Scheduling

- **Built-in** (default): `poll_interval` is a Go duration (`3h` default; a value
  under `1h` is clamped up to `1h`). A cycle runs on start, then every interval.
  It is the single cadence for both the findings loop and the Torznab feed.
- **External / resident-idle**: set `poll_interval: off` (or `disabled` / `0`).
  The daemon runs no internal timer; the container idles healthy and an external
  scheduler drives each cycle with the `poll` subcommand, which runs one cycle,
  updates the health marker, and exits `0` or `1`. The Torznab feed is served from
  the last cycle's snapshot, so it is empty until the first `poll` runs. With
  [Ofelia](https://github.com/mcuadros/ofelia), label the service:

  ```yaml
      labels:
        ofelia.enabled: "true"
        ofelia.job-exec.seadex-poll.schedule: "@every 3h"
        ofelia.job-exec.seadex-poll.command: "/seadex-scout poll"
  ```

  Any scheduler works: `docker exec seadex-scout /seadex-scout poll` is the whole
  contract.

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
- `unverified`: files are present but no release group could be identified.

A trailing **`not_on_seadex`** section then lists the library items recognized as
anime (through the Fribb catalogue) that SeaDex does not list at all, so you can
see which of your titles SeaDex has not curated. Every row links the
Sonarr/Radarr item, the SeaDex entry, and each best release.

Each run writes a timestamped pair into `report.dir` (default
`/config/reports`): `report-<UTC date+time>.md` grouped by verdict and
`report-<UTC date+time>.json` beside it, plus one `report item` log line per
anime. Successive runs never overwrite one another, and the app deletes no
reports, so prune old pairs yourself. A second report started while one is still
running refuses with `another report is already running` and exits `1`.

While the daemon runs, produce a new report without stopping it:

```sh
docker exec seadex-scout /seadex-scout report
```

A report never writes the state cache, so it is safe to run alongside a daemon
cycle. To produce reports on a schedule, use the same Ofelia `job-exec` pattern as
above with `/seadex-scout report`. The output of a `docker exec` run goes to the
exec session, not the container log stream, so Loki never sees its `report item`
lines.

> When you run `report` as the container's command (rather than `docker exec` into
> the running daemon), disable the image's baked healthcheck for that one-shot
> container (compose: `healthcheck: { disable: true }`; docker run:
> `--no-healthcheck`). The health marker belongs to the daemon's poll loop, so a
> report-only container reads unhealthy while the report is still generating, and
> an unhealthy-restart watchdog could kill it mid-run.

## Indexer (Torznab feed)

When a Prowlarr Torznab URL is configured, the daemon serves a
[Torznab](https://torznab.github.io/spec-1.3-draft/) feed of SeaDex releases for
Sonarr/Radarr, alongside the compare loop in the same process. It is the opt-in
automation path: unlike the report-only findings, it lets the arrs grab. Point
your arrs at it (directly or through Prowlarr) and they parse, match, and grab
through their own engines, profiles, and history, exactly as for any other
indexer. To set it up, see [docs/torznab-indexer.md](docs/torznab-indexer.md).

The feed handles its two request kinds two different ways. A **search** (the arr's
automatic or interactive search, which carries a query) is proxied to Prowlarr's
Nyaa and AnimeBytes Torznab endpoints and filtered to what SeaDex curates, so its
download links are Prowlarr's own and no tracker passkey is needed here. If every
upstream query fails (Prowlarr unreachable), the search answers with a Torznab
`<error code="900">` document rather than an empty feed, so the arr records a
failed search instead of concluding there were no results. A **periodic RSS
check** (the no-query "recent releases" fetch the arrs run on their sync interval)
has no query to match against, so the feed synthesizes the SeaDex list itself: one
item per curated release, its title taken from SeaDex's own file names, with a
public Nyaa `.torrent` link or an AnimeBytes link built from your `ab_passkey`.

Every item, either way, carries a **download-volume-factor marker**: SeaDex's
_best_ release is tagged `0.75` (which the arrs read as AnimeBytes Freeleech25)
and an _alt_ `0.25` (Freeleech75). That marker is the signal you map to a Custom
Format, which is what makes the arrs prefer SeaDex's pick. Each item's category is
the entry's real media type, resolved from the anime-list mapping rather than
guessed from the file name: a film is `2000` (Movies → Radarr), while a series,
OVA, or special is `5070` (Anime → Sonarr), so a single-file special is never
mistaken for a movie.

**It answers whole-season searches, not per-episode ones.** SeaDex tracks season
packs, so the feed answers a season search with the pack and returns nothing,
without contacting a tracker, for a per-episode query. Specials and movies are
single releases and are always answered.

> **Setup requirement:** the feed relies on the season search, so enable **Anime
> Standard Format Search** on the seadex-scout indexer in Sonarr (Settings →
> Indexers → the indexer). Without it, Sonarr sends only per-episode queries,
> which the feed does not answer.

## Security

The feed is gated by `feed_api_key`: a request without the matching `apikey` gets
`401`. Its links are Prowlarr proxy URLs (for searches) and, for the AnimeBytes
RSS feed, direct AnimeBytes links that embed your `ab_passkey`. Treat the endpoint
as sensitive and keep it on your LAN; behind an internal reverse proxy is fine
(that is what the per-tracker subdomain routing is for), but do not put it on the
public internet. seadex-scout sends the Prowlarr API key in a request header,
never in a logged URL, and never writes it to the logs.

The synthesized feed is also persisted on disk between cycles as
`/config/feed.json`, and its AnimeBytes items embed the `ab_passkey` in their
download links. The file is written owner-only (`0600`), but treat it as
secret-bearing: a `/config` backup captures the passkey even when your
`config.yaml` only references it through `${SEADEX_SCOUT_AB_PASSKEY}`.

## How matching works

SeaDex keys everything on AniList IDs; Sonarr keys on TVDB, Radarr on TMDB/IMDb.
seadex-scout bridges them:

- **ID mapping.** The Fribb `anime-list-mini.json` dataset maps `anilist_id` to
  `type` (TV vs movie), `tvdb_id`, `themoviedb_id`, and `imdb_id`. Each cycle
  revalidates it with a conditional GET, so an unchanged multi-MB file is a cheap
  `304` and is never re-downloaded. The `type` decides which arr and which ID
  field to use.
- **Overrides.** To pin the entries Fribb misses, drop a `/config/overrides.json`
  beside the config: a JSON array of records keyed by `anilist_id`, applied ahead
  of Fribb. Absent is fine. Fields per record: `anilist_id` (required), `type`
  (`movie` routes to Radarr, anything else to Sonarr), `tvdb_id`, `tmdb_movies`
  (array of ints), `imdb_ids` (array of strings), and `season_tvdb`. These are
  NOT the upstream Fribb field names (`imdb_id`, `themoviedb_id`, `season`), which
  are ignored with a warning naming the key. An override **replaces** the whole
  mapping record for its `anilist_id` (no field-by-field merge), so when
  correcting an entry Fribb already has, restate every field the entry needs.
- **Title fallback.** When an entry maps through neither, seadex-scout fetches its
  titles and format from AniList and tries a conservative normalized
  title-plus-year match against the library: exact match, single candidate
  required, and an ambiguous match is skipped rather than guessed. Mapped items
  never reach AniList, so steady-state AniList traffic is near zero.

## Release classification and filters

Each SeaDex release and each library file is classified into one vocabulary:
release group, tracker (public like Nyaa, private like AnimeBytes), resolution,
codec (x265/x264), dual-audio, and **kind** (`remux` / `encode` / `unknown`). The
remux-vs-encode decision reads names and notes, never a size or bitrate
inference; an unclassifiable release is `unknown` and is never silently dropped.
The comparison is **group-centric**: an item is aligned when a recommended
release group is already present on it.

These filters shape the findings and the report only. The
[indexer](#indexer-torznab-feed) feed applies none of them; there the arrs filter
through their own quality profile and Custom Formats. All are optional:

- `filters.exclude_remux` (default false): when true, releases classified `remux`
  never count as a recommendation. The default keeps them, because on SeaDex a
  remux is often the best release.
- `filters.require_dual_audio` (default false): drop releases that are not
  dual-audio.
- `filters.exclude_specials` (default false): when true, drop OVA/ONA/special
  entries from findings and the report.
- `animebytes` (default false): the one tracker knob, at the top level because it
  is tracker access rather than a content filter. The public trackers SeaDex lists
  (Nyaa, AnimeTosho, RuTracker) are always considered; the private tracker
  AnimeBytes is included only when you turn this on, which means you have an
  account. On, a finding carries every source, so a release on both Nyaa and
  AnimeBytes shows both links. Because seadex-scout only links, an AnimeBytes link
  is the torrent page you open as a member: no tracker credentials are needed.
- `arr_tags.include` / `arr_tags.exclude` (arr-side): scan only items carrying an
  include tag, and never items carrying an exclude tag; an exclude wins when an
  item has both.

## Configuration

All configuration lives in one YAML file, `/config/config.yaml` (override the path
with `CONFIG_PATH`). On first boot with no config, seadex-scout writes a commented
starter there and exits with a warning; edit it and restart. The full annotated
template is [`config.example.yaml`](config.example.yaml).

Any string value can reference `SONARR_*`, `RADARR_*`, or `SEADEX_SCOUT_*`
environment variables with `${VAR}`, so secrets can live in an `.env` or a Docker
secret instead of the file. API keys are never logged (only whether each is set).

```yaml
sonarr:
  enabled: true
  url: "http://sonarr:8989"
  api_key: "${SONARR_API_KEY}"    # or paste it directly; required when enabled
  public_url: ""                  # browser URL for report deep-links; falls back to url
radarr:
  enabled: false
  url: "http://radarr:7878"
  api_key: ""

mode: "daemon"                    # daemon (scheduled) | report (one-shot, exit)
poll_interval: "3h"               # cadence for BOTH findings + Torznab feed; off/disabled/0 = external trigger via `poll`

animebytes: false                 # true if you have an AnimeBytes account: adds AB releases + links

filters:
  exclude_remux: false            # true drops remuxes (default keeps them)
  require_dual_audio: false
  exclude_specials: false         # true drops OVA/ONA/specials

arr_tags:
  include: []                     # only scan arr items with these tags; [] = all
  exclude: []                     # never scan arr items with these tags (an exclude wins)

report:
  dir: "/config/reports"          # timestamped report-<UTC date+time>.md + .json written here

indexer:                          # the daemon serves the feed whenever a Torznab URL is set below
  feed_api_key: ""                # the arrs send this; the feed checks it (openssl rand -hex 16)
  nyaa_torznab_url: ""            # Prowlarr Nyaa Torznab URL, e.g. http://prowlarr:9696/1/api ("" = off)
  ab_torznab_url: ""              # Prowlarr AnimeBytes Torznab URL ("" = off)
  prowlarr_api_key: ""            # Prowlarr API key; secret, never logged
  ab_passkey: ""                  # AnimeBytes passkey for the AB RSS feed download links ("" = AB RSS off; Nyaa needs none)

log:
  level: "info"                   # debug | info | warn | error
  format: "json"                  # json | text
```

At least one arr must be `enabled` with a `url` and an `api_key`; an enabled arr
missing either is a configuration error. An unknown or misplaced key is also
rejected at startup with an error naming it (`unknown configuration key
"anime_bytes"`), so a typo fails fast instead of being silently ignored.
`public_url` is the browser-facing base used only for the report's deep-links
(leave it empty to reuse `url`), so an internal Docker hostname in `url` still
yields working links.

The upstream endpoints (SeaDex, Fribb, AniList), their request cadences, and the
internal file locations under `/config` (the state cache, the reports, and the
overrides file) are fixed and are not config keys, so the file stays limited to
what you actually tune.

## Observability

Observability is slog-only: no metrics endpoint, and no HTTP surface unless you
configure the [indexer](#indexer-torznab-feed) feed, the only thing that binds a
port (fixed at `:9118`). An alert-only deployment stays socket-less.

- **slog to Loki.** A JSON handler writes to stdout; Alloy (or any collector)
  ships it to Loki. A finding is one line at `warn` (`msg="better release
  available"`) with `title`, `al_id`, `arr`, `current_group`,
  `recommended_group`, `tracker`, `resolution`, `kind`, `classification_reason`,
  a headline `release_url`, and `release_urls` (every obtainable source, so a
  release on both Nyaa and AnimeBytes carries both). Informational cases
  (`incomplete`, `theoretical_best`, `mixed_group_manual`) log at `info`. Each
  cycle closes with a completion line: `cycle complete` (carrying the counts,
  mapping coverage, and AniList usage) when healthy, or `cycle degraded` at `warn`
  with a `reason` when an upstream outage or a safety guard skipped the
  comparison. Report mode emits one `report item` line per anime.
- **Health.** The distroless image's Docker `HEALTHCHECK` runs the
  `seadex-scout health` subcommand against a `/tmp/.healthy` file marker, so it
  needs no shell and no port; the marker reflects the last cycle's library-ingest
  outcome.

## Alerting

seadex-scout ships no notifier of its own; its operational state is in its logs
(there is no metrics endpoint). Ship the container's logs to Loki (Grafana
Alloy's Docker log discovery does this with no configuration) and evaluate the
rules in [`alerts.yaml`](alerts.yaml) with
[Loki's ruler](https://grafana.com/docs/loki/latest/alert/); firing alerts
deliver through your Alertmanager like any Prometheus metric alert. They cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `SeadexScoutCycleError` | a cycle logs an error: the Sonarr/Radarr library walk failed, or a library-shrink / mapping-refresh guard escalated after 8 consecutive degraded cycles | warning |
| `SeadexScoutScanStalled` | no cycle completion line (`cycle complete` or `cycle degraded`) in 7h, so the daemon poll loop is wedged | warning |
| `SeadexScoutBetterReleaseFound` | SeaDex recommended a better release than the one on disk (informational, not a fault) | info |
| `SeadexScoutReportWritten` | a report run wrote a season-level alignment report (informational) | info |

Thresholds and the `severity` labels are starting points. Adjust the `container`
selector (or `job` / `service`, depending on your log collector) to your
deployment and the stall window to your `poll_interval` (default 3h). In
resident-idle (`poll_interval: off`) or report mode each cycle runs through a
`docker exec` child (the `poll` / `report` subcommand), so its logs go to the
trigger rather than the container's log stream and the log rules cannot fire;
alert on your external scheduler's own job result instead. The rules assume the
default JSON log handler; for `log.format: text`, swap the
`| json | level="ERROR"` parser stage for a `|= "level=ERROR"` line filter. Route
by whatever labels your Alertmanager uses.

## Deployment

seadex-scout ships as a distroless, non-root, multi-arch (amd64 + arm64) image at
`ghcr.io/cplieger/seadex-scout`. The [`compose.yaml`](compose.yaml) at the repo
root is a working example. For a hardened deployment, layer on:

```yaml
    read_only: true
    cap_drop: ["ALL"]
    security_opt: ["no-new-privileges:true"]
    tmpfs: ["/tmp:size=1m,mode=1777,noexec,nosuid,nodev"]  # backs the health marker
```

The `/config` volume must be writable by the container user (set `user:` to match
the host owner of the mounted directory); it holds `config.yaml`, the state cache,
and the report output. By default the container exposes no network port:
observability is slog-only and health is the file-marker `health` subcommand. It
binds a port only when you configure the [indexer](#indexer-torznab-feed) feed,
which serves on a fixed `:9118`; publish that port only on your LAN.

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for
the repo layout, the conventions, and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0. Linking [`arrapi`](https://github.com/cplieger/arrapi) (GPL-3.0) makes
seadex-scout GPL-3.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

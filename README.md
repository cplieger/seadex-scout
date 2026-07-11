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
get, and you decide. (The daemon can also publish SeaDex as a
[Torznab feed](#indexer-torznab-feed) for Sonarr/Radarr to grab from — configure
it when you want automation through the arrs.)

## Three features, one binary

Everything runs from one image and one config file:

1. **Monitoring & alerting** (always on) — the daemon continuously compares your
   library to SeaDex and logs a `warn` finding whenever a better release exists
   than the one on disk. You turn those log lines into Loki/Grafana alerts (see
   [Alerting](#alerting)); the app ships no notifier of its own.
2. **On-demand report** — a season-by-season audit of how your whole library
   lines up with SeaDex (`have_best` / `have_alt` / `have_unlisted` / …), written
   as Markdown + JSON, for catching up an existing library. See
   [The report](#the-report).
3. **Torznab feed** (optional) — publishes SeaDex's picks as a feed Sonarr/Radarr
   grab from through their own engine, for hands-off upgrades. Off until you point
   it at Prowlarr. See [Indexer (Torznab feed)](#indexer-torznab-feed).

The first two are report-only and keep a human in the loop; the third is the
opt-in automation path. The rest of this README details each.

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
   (remux policy, resolution floor, AnimeBytes on/off, dual-audio).
4. Compares the surviving recommendation against what you have and, when SeaDex
   has something better, emits a `warn` log line.

It is cheap to run and observable: it caches the library walk and the ID map,
keeps AniList traffic near zero, and logs mapping coverage each cycle so misses
are visible rather than silent.

## Run modes

The `mode` setting (or a subcommand) picks the run mode:

- **daemon** (default): the poll loop above, flagging better releases as findings
  on the log (slog to Loki). It runs a cycle on start and every `poll_interval`,
  or sits resident-idle when `poll_interval` is `off` (see Scheduling below). When
  a Prowlarr Torznab URL is configured, the same daemon also serves the
  [Torznab feed](#indexer-torznab-feed) for Sonarr/Radarr — both features in one
  process, no extra knob.
- **report**: a one-shot, read-only audit. It scans the whole library once,
  writes a SeaDex-alignment report, and exits. Trigger it as the container
  command (`report`), by setting `mode: report` in the config, or, while the
  daemon runs, via `docker exec`.

### Scheduling

The daemon follows the standard `*_INTERVAL` scheduling shape:

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
grouped by verdict, a JSON file alongside it, and one `report item` slog line per
anime (queryable in Loki like the daemon findings). Each run writes a timestamped
`report-<UTC time>.md` + `.json` pair into `report.dir` (default
`/config/reports`), so successive reports never overwrite one another.

While the daemon runs, produce a fresh report without stopping it by running the
`report` subcommand in the container:

```sh
docker exec seadex-scout /seadex-scout report
```

The `.md` and `.json` land in `report.dir` on the `/config` volume, where you
read them directly. A report never writes the state cache, so it is safe to run
alongside a daemon cycle. To produce reports on a schedule, use the same
Ofelia `job-exec` pattern as above with `/seadex-scout report`.

## Indexer (Torznab feed)

When a Prowlarr Torznab URL is configured, the daemon serves a
[Torznab](https://torznab.github.io/) feed of SeaDex releases for Sonarr/Radarr,
alongside the compare loop in the same process. It is the opt-in automation path:
unlike the report-only findings, it lets the arrs grab. Point your arrs at it
(directly or through Prowlarr) and they parse, match, and grab through their own
engines, profiles, and history, exactly as for any other indexer.

Rather than synthesize release data, the feed **proxies Prowlarr** and filters to
SeaDex's curation. On each query it asks Prowlarr's per-indexer Torznab endpoints
for **Nyaa** and **AnimeBytes**, keeps only the results SeaDex tracks (matched by
the tracker id in each release's page URL, with the info hash as a fallback), and
passes their real data straight through — real title, seeders, size, and
Prowlarr's own proxied download link. Because the download link is Prowlarr's,
**no tracker passkey lives here**: Prowlarr proxies the grab using the AnimeBytes
credentials it already holds. The one thing the feed adds is a
**download-volume-factor marker**: SeaDex's _best_ release is tagged `0.75` (which
the arrs read as AnimeBytes Freeleech25) and an _alt_ `0.25` (Freeleech75) — the
signal you map to a Custom Format (below).

Its advertised capabilities mirror the Nyaa and AnimeBytes indexer definitions:
`q`-based search (`t=search`, `t=tvsearch` with `season`/`ep`, `t=movie`; neither
tracker supports id-based search), and the TV/Anime (`5070`) and Movies (`2000`)
categories.

**It answers season searches, not per-episode ones.** Sonarr searches an anime
season both as a whole season _and_ episode by episode (per Sonarr's
`NewznabRequestGenerator`), and SeaDex tracks season packs — so the feed answers
the season search (returning the pack) and deliberately returns **empty, without
contacting any tracker, for a per-episode query** (a `tvsearch` with an `ep`, or a
`search` whose title ends in an absolute episode number like `Frieren 01`). That
spares the trackers a query per episode per title alias, and makes a manual
single-episode search free. Specials and movies are single releases, so they are
always answered. The SeaDex catalogue of _what_ is curated is cached and refreshed
in the background; the endpoint comes up immediately, so an arr's "Test"
(`t=caps`) succeeds while the first SeaDex fetch warms up.

> **Setup requirement:** because the feed relies on the season search, enable
> **Anime Standard Format Search** on the seadex-scout indexer in Sonarr
> (Settings → Indexers → the indexer → Anime Standard Format Search). That option
> is what makes Sonarr issue the `q={title}&season={s}` season query for
> Anime-type series; without it Sonarr only sends per-episode queries, which the
> feed does not answer.

### Enabling it — step by step

There is no separate command or container: the daemon starts the feed the moment
you give it a Prowlarr Torznab URL. Leave both URLs empty and the daemon binds no
HTTP port at all, staying socket-less for an alert-only deployment.

**1. Point the feed at Prowlarr.** In Prowlarr, add **Nyaa** and **AnimeBytes** as
indexers if you haven't. Each indexer's page shows its **Torznab Url** (like
`http://prowlarr:9696/1/api`); copy both, and grab a Prowlarr API key from
Prowlarr → Settings → General. Fill in the `indexer` section and restart:

```yaml
indexer:
  listen: ":9118"                                   # host:port to bind; keep it LAN-only
  api_key: "a-random-string"                        # the arrs send this; the feed checks it
  nyaa_torznab_url: "http://prowlarr:9696/1/api"    # "" disables Nyaa
  ab_torznab_url: "http://prowlarr:9696/2/api"      # "" disables AnimeBytes
  prowlarr_api_key: "${SEADEX_SCOUT_PROWLARR_KEY}"  # secret, never logged
```

The download links the feed serves are Prowlarr's own proxy URLs, so **no tracker
passkey lives here** — Prowlarr grabs with the AnimeBytes credentials it already
holds.

**2. Add the feed to Sonarr/Radarr.** Settings → Indexers → Add → **Torznab**
(Custom):

- **URL:** `http://seadex-scout:9118/api`
- **API Key:** the `indexer.api_key` from step 1
- **Categories:** `5070` (Anime) in Sonarr, `2000` (Movies) in Radarr
- **☑ Anime Standard Format Search — required.** This is what makes Sonarr issue
  the whole-season query the feed answers. Without it Sonarr sends only
  per-episode queries, which the feed intentionally ignores, and you get nothing.

You can instead add it to Prowlarr and let Prowlarr sync it to the arrs — either
works, there is no query loop — but the Anime Standard Format Search toggle must
still end up set on the indexer as the arr sees it.

**3. Create two Custom Formats.** Settings → Custom Formats → Add. Each gets a
single condition of type **Indexer Flag**:

| Custom Format | Condition (Indexer Flag) | Matches | From `downloadvolumefactor` |
| --- | --- | --- | --- |
| `SeaDex (best)` | `Freeleech25` | SeaDex's best pick | `0.75` |
| `SeaDex (alt)` | `Freeleech75` | a SeaDex-listed alt | `0.25` |

(Leave the condition's "negate" and "required" boxes unchecked. The flag names are
exactly `Freeleech25` and `Freeleech75` — Sonarr/Radarr derive them from the
feed's `downloadvolumefactor`, confirmed against the arr source.)

**4. Score them on your anime quality profile — and _only_ that profile.**
Settings → Profiles → your anime profile → Custom Formats: give `SeaDex (best)` a
high positive score (e.g. `100`) and `SeaDex (alt)` a lower one (e.g. `50`).
Sonarr/Radarr now prefer — and upgrade to — SeaDex's pick over an equivalent
non-SeaDex release. **Scoping the scores to the anime profile matters:** it keeps
the markers from colliding with genuine AnimeBytes Freeleech25/75 releases in your
non-anime libraries.

By design, AnimeBytes sees the feed's season query in addition to the arr's own
direct query — one extra search per season. Per-episode searches are answered
from nothing, without a tracker query, so they add no load.

### Why a download-volume-factor marker (not a title tag)

The marker is the one SeaDex-specific signal the feed injects, and the choice is
deliberate. A tag in the release _title_ would not survive: the arrs rewrite the
release name on import (SceneName), so a title marker is lost and the release
stops being recognizable, which can trigger re-grab loops. `downloadvolumefactor`
instead becomes an **indexer flag** the arrs persist on the grabbed file's
history — so the Custom Format keeps matching for the life of the file, with no
loop.

### Security

The feed is gated by `api_key` (a request without the matching `apikey` gets
`401`). The download links it serves are Prowlarr proxy URLs, so treat the
endpoint as sensitive — **bind it to your LAN and never expose it publicly** (no
public reverse-proxy hostname). The Prowlarr API key is sent to Prowlarr in a
request header (never in a logged URL) and is never written to the logs.

## How matching works

SeaDex keys everything on AniList IDs; Sonarr keys on TVDB, Radarr on TMDB/IMDb.
seadex-scout bridges them:

- **ID mapping.** The Fribb `anime-list-mini.json` dataset maps `anilist_id` to
  `type` (TV vs movie), `tvdb_id`, `themoviedb_id`, and `imdb_id`. It is fetched
  with a conditional GET on a slow cadence and cached, so an unchanged multi-MB
  file is never re-downloaded. The `type` decides which arr and which ID field
  to use.
- **Overrides.** Drop a `/config/overrides.json` (a JSON array of records keyed
  by `anilist_id`) beside the config to pin the entries Fribb misses; it is
  applied ahead of Fribb. Absent is fine.
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
- `filters.animebytes` (default false) is the one tracker knob. SeaDex lists
  releases on just two trackers: public **Nyaa** and private **AnimeBytes**. The
  public trackers (Nyaa, AnimeTosho, RuTracker) are always considered; AnimeBytes
  is included only when you turn this on, i.e. you have an account. Off, AB
  releases are invisible; on, a finding carries every source, so a release on
  both Nyaa and AB shows both links. Because seadex-scout only links (never
  downloads), an AB link is just the torrent page you open as a member: no
  tracker API keys or credentials are needed.
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
  url: "http://sonarr:8989"
  api_key: "${SONARR_API_KEY}"    # or paste it directly; required when enabled
  public_url: ""                  # browser URL for report deep-links; falls back to url
radarr:
  enabled: false
  url: "http://radarr:7878"
  api_key: ""

mode: "daemon"                    # daemon (scheduled) | report (one-shot, exit)
poll_interval: "12h"              # Go duration; off/disabled/0 = external trigger via `poll`
season_scoping: false             # compare per-season instead of series-level

filters:
  allow_remux: false
  min_resolution: "1080p"         # e.g. 1080p, 720p; "" = no floor
  require_dual_audio: false
  animebytes: false               # true if you have an AnimeBytes account: adds AB releases + links
  remux_groups: []
  include_specials: true

tags:
  include: []                     # only scan arr items with these tags; [] = all
  exclude: []

report:
  dir: "/config/reports"          # timestamped report-<UTC time>.md + .json written here

indexer:                          # only used by the `indexer` subcommand
  listen: ":9118"                 # host:port for the Torznab feed; keep it LAN-only
  api_key: ""                     # the arrs send this; the feed checks it
  nyaa_torznab_url: ""            # Prowlarr Nyaa Torznab URL (e.g. http://prowlarr:9696/1/api)
  ab_torznab_url: ""              # Prowlarr AnimeBytes Torznab URL
  prowlarr_api_key: ""            # Prowlarr API key; secret, never logged

log:
  level: "info"                   # debug | info | warn | error
  format: "json"                  # json | text
```

At least one arr must be `enabled` with a `url` + `api_key`; an enabled arr
missing either is a configuration error. `public_url` is the browser-facing base
used only for the report's deep-links (leave empty to reuse `url`), so an
internal Docker hostname in `url` still yields working links.

The upstream endpoints (SeaDex, Fribb, AniList), their request cadences, and the
internal file locations (the state cache and the reports and overrides files) are
fixed under `/config` and are not config keys, so the file stays limited to what
you actually tune. To pin a mapping Fribb gets wrong, drop a
`/config/overrides.json` beside the config (see [How matching works](#how-matching-works)).

## Observability

Observability is slog-only: no metrics endpoint, and no HTTP surface unless you
configure the [indexer](#indexer-torznab-feed) feed — the only thing that binds a
port (`indexer.listen`). An alert-only deployment stays socket-less.

- **slog to Loki.** A JSON handler writes to stdout; Alloy (or any collector)
  ships it to Loki. A finding is one line at `warn` (`msg="better release
  available"`) with `title`, `al_id`, `arr`, `current_group`,
  `recommended_group`, `tracker`, `resolution`, `kind`, `classification_reason`,
  a headline `release_url`, and `release_urls` (every obtainable source, so a
  release on both Nyaa and AnimeBytes carries both). Informational cases
  (`incomplete`, `theoretical_best`, `mixed_group_manual`) log at `info`. Each
  cycle closes with a `cycle complete` line carrying the counts, mapping
  coverage, and AniList usage; report mode emits one `report item` line
  per anime.
- **Alert rules.** seadex-scout ships no notifier of its own; see
  [Alerting](#alerting) for reference Loki ruler rules you can copy (a cycle
  fault, a poll-loop deadman, and an informational better-release rule).
- **Health.** The distroless image's Docker `HEALTHCHECK` uses the
  `seadex-scout health` subcommand (a `/tmp/.healthy` file marker), so no shell or
  port is needed; the marker reflects the last cycle's library-ingest outcome.

## Alerting

seadex-scout ships no notifier of its own; its operational state is in its logs
(there is no metrics endpoint). Ship the container's logs to Loki (Grafana
Alloy's Docker log discovery does this with no configuration) and evaluate the
rules in [`alerts.yaml`](alerts.yaml) with
[Loki's ruler](https://grafana.com/docs/loki/latest/alert/); firing alerts
deliver through your Alertmanager like any Prometheus metric alert. They cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `SeadexScoutCycleError` | a cycle logs an error, e.g. the Sonarr/Radarr library walk failed and the cycle is marked unhealthy | warning |
| `SeadexScoutScanStalled` | no `cycle complete` line in 26h, i.e. the daemon poll loop is wedged | warning |
| `SeadexScoutBetterReleaseFound` | SeaDex recommended a better release than the one on disk (informational, not a fault) | info |

Thresholds and the `severity` labels are starting points. Adjust the `container`
selector (or `job` / `service`, depending on your log collector) to your
deployment and the stall window to your `poll_interval` (default 12h). In
resident-idle (`poll_interval: off`) or report mode each cycle runs via a
`docker exec` child (the `poll` / `report` subcommand), so its logs go to the
trigger rather than the container's log stream and the log rules cannot fire;
alert on your external scheduler's own job result instead. The rules assume
the default JSON log handler; for `log.format: text`, swap the
`| json | level="ERROR"` parser stage for a `|= "level=ERROR"` line filter. Route
by whatever labels your Alertmanager uses.

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
cache, and the report output. By default the container exposes no network
port — observability is slog-only and health is the file-marker `health`
subcommand. It binds a port only when you configure the
[indexer](#indexer-torznab-feed) feed, which serves on `indexer.listen` (default
`:9118`); publish that port only on your LAN.

## Report-only, on purpose

seadexarr's value proposition is the opposite of this one: it grabs, hands-free,
into a download client. seadex-scout keeps a human in the loop deliberately.
SeaDex recommendations are curation judgments that interact with storage budget,
tracker access, and per-show taste; a nudge you act on beats an automated grab.
Every finding carries the exact SeaDex and tracker links so acting on it is easy.
Direct auto-grab — seadex-scout pushing to a download client, as seadexarr does —
remains out of scope. The opt-in [indexer](#indexer-torznab-feed) takes the
middle path instead: it hands the releases to Sonarr/Radarr as a feed and lets
them grab through their own engine, profiles, and history, so nothing bypasses
the tooling you already trust.

## License

GPL-3.0. Linking [`arrapi`](https://github.com/cplieger/arrapi) (GPL-3.0) makes
seadex-scout GPL-3.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

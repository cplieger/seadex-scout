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
the one on disk. It emits a structured log line for each finding (slog to Loki).
It never downloads, grabs, or touches a torrent client: it tells you what to go
get, and you decide. (The daemon can also publish SeaDex as a
[Torznab feed](#indexer-torznab-feed) for Sonarr/Radarr to grab from; configure
it when you want automation through the arrs.)

## Three features, one binary

Everything runs from one image and one config file:

1. **Monitoring & alerting** (always on): the daemon logs a `warn` finding
   whenever SeaDex has a better release than the one on disk; you alert on
   those lines (see [Alerting](#alerting)).
2. **On-demand report**: a season-by-season audit of the whole library
   (`have_best` / `have_alt` / `have_unlisted` / …), for catching up an
   existing library. See [The report](#the-report).
3. **Torznab feed** (optional): publishes SeaDex's picks for Sonarr/Radarr to
   grab through their own engine. Off until you point it at Prowlarr. See
   [Indexer (Torznab feed)](#indexer-torznab-feed).

The first two are report-only and keep a human in the loop; the third is the
opt-in automation path.

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

On start, and once a day after that, seadex-scout runs a **full pass**:

1. Walks the Sonarr/Radarr anime library (with arr-side tag include/exclude) and
   fingerprints each item's current release (group, resolution, codec,
   remux-vs-encode, dual-audio).
2. Matches each SeaDex entry to a library item by **AniList ID** through the
   [Fribb anime-lists](https://github.com/Fribb/anime-lists) ID bridge, with an
   **AniList title fallback** for the entries that do not map.
3. Classifies and filters SeaDex's recommended releases by your preferences
   (remux policy, AnimeBytes on/off, dual-audio).
4. Compares the surviving recommendation against what you have and, when SeaDex
   has something better, emits a `warn` log line.

When the [Torznab feed](#indexer-torznab-feed) is configured, the same cycle also
rebuilds it from that one SeaDex fetch, so a finding and what the arrs can grab
from the feed always reflect the same refresh.

## Run modes

The `mode` setting (or a subcommand) picks the run mode:

- **daemon** (default): the poll loop above, flagging better releases as
  findings on the log. When a Prowlarr Torznab URL is configured, the same
  daemon also serves the [Torznab feed](#indexer-torznab-feed); both features
  run in one process.
- **report**: a one-shot, read-only audit. It scans the whole library once,
  writes a SeaDex-alignment report, and exits. Trigger it as the container
  command (`report`), by setting `mode: report` in the config, or, while the
  daemon runs, via `docker exec`.

### Scheduling

The daemon follows the standard `*_INTERVAL` scheduling shape:

- **Built-in** (default): `poll_interval` is a Go duration (`15m` default,
  clamped to `15m`–`720h`). It is the single cadence for both the alert loop and
  the Torznab feed, and most iterations are a cheap **tick** — see
  [Freshness](#freshness) below.
- **External / resident-idle**: set `poll_interval: off` (or `disabled` / `0`).
  There is no internal timer; the container idles healthy and an external
  scheduler drives each cycle via the `poll` subcommand, which runs one cycle,
  updates the health marker, and exits `0`/`1`. Concurrent triggers coalesce on
  a cross-process lock: a `poll` arriving while a cycle is in flight queues one
  rerun and exits `0` immediately. The Torznab feed, when configured, refreshes
  on that same `poll` (it is served from the last cycle's snapshot, so it is
  empty until the first `poll` runs). With
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
- `unverified`: the release-group evidence is unknown on one side (an on-disk
  file with no identifiable group, or an untagged SeaDex release), so the
  verdict could not be determined: a manual check, not a verdict. This also
  covers the case where SeaDex's best releases are provenly absent but its
  _alt_ releases are untagged: you do not have a best release, but whether the
  one you have is a listed alt or unlisted cannot be told apart.

A trailing `not_on_seadex` section lists the library items recognized as anime
that SeaDex does not curate at all. A best release you could not act on (no
usable link, or a tracker you cannot use) stays visible but never drives the
verdict; the report marks it unobtainable.

Each run writes a timestamped `report-<UTC>.md` + `.json` pair into `report.dir`
(default `/config/reports`), so successive reports never overwrite one another.
Reports are never deleted by the app; prune old pairs yourself (from the host;
the distroless image has no shell). They are written owner-only (`0600`,
new directories `0700`): a report enumerates your library and can carry
private-tracker links.

While the daemon runs, produce a fresh report without stopping it:

```sh
docker exec seadex-scout /seadex-scout report
```

A report never writes the state cache, so it is safe alongside a daemon cycle.
A `docker exec` run's output goes to the exec session, not the container log
stream, so Loki never sees those `report item` lines. To schedule reports, use
the Ofelia `job-exec` pattern from Scheduling with `/seadex-scout report`.

> When you run `report` as the container's command (rather than `docker exec`),
> disable the image's baked healthcheck for that one-shot container (compose:
> `healthcheck: { disable: true }`) and give it no restart policy
> (`restart: "no"`): the health marker belongs to the daemon's poll loop, and a
> restart-policied one-shot loops, writing a new report pair on every restart.

## Indexer (Torznab feed)

When a Prowlarr Torznab URL is configured, the daemon serves a
[Torznab](https://torznab.github.io/) feed of SeaDex releases for Sonarr/Radarr,
alongside the compare loop in the same process. It is the opt-in automation path:
unlike the report-only findings, it lets the arrs grab. Point your arrs at it
(directly or through Prowlarr) and they parse, match, and grab through their own
engines, profiles, and history, exactly as for any other indexer.

Behavior you should know before wiring it up:

- **Searches** are proxied to Prowlarr's Nyaa and AnimeBytes endpoints and
  filtered to what SeaDex curates, with real titles, seeders, and
  Prowlarr-proxied download links; a search needs no tracker passkey here.
- **The RSS feed** lists releases newly curated by SeaDex, aging out after
  14 days. It starts **empty** on first run and grows as SeaDex curates:
  catching up an existing library is what searches and [the report](#the-report)
  are for. A release curated before the feed began surfaces only through a
  search.
- **RSS items report `seeders=1`** (the feed has no live swarm data), so a dead
  torrent can still look grabbable and stall in your download queue.
  Minimum-seeder rejection only has real data on search results.
- **It answers whole-season searches, not per-episode ones** (SeaDex tracks
  season packs), which spares the trackers a query per episode. This makes the
  **Anime Standard Format Search** toggle mandatory in Sonarr: without it,
  Sonarr sends only per-episode queries and gets nothing (step 2 below).

### Enabling it, step by step

There is no separate command or container: the daemon serves the feed whenever a
Prowlarr Torznab URL is configured, and binds no HTTP port otherwise.

**1. Point the feed at Prowlarr.** Add **Nyaa** and **AnimeBytes** as Prowlarr
indexers if you haven't, and fill the config's `indexer` section: their two
Torznab URLs, a Prowlarr API key, and (for the AB RSS feed) your `ab_passkey`;
the `feed_api_key` is already filled in — the starter config written on first
boot generates a fresh 32-character key, so you only need to copy it into the
arrs. Keys in
[Configuration reference](#configuration-reference). The one Prowlarr setting
that matters: leave the Nyaa indexer's **Sort requested from site** at its
default (created/date, descending); the feed's title lookups depend on that
ordering. The feed's port is fixed at `:9118`.

**2. Add the feed to Sonarr/Radarr.** Settings → Indexers → Add → **Torznab**
(Custom). Adding it through Prowlarr with a sync profile works too; the toggles
below must end up on the indexer as the arr sees it.

- **URL:** the feed is **per-tracker**: add Nyaa as
  `http://seadex-scout:9118/nyaa` and AnimeBytes as `http://seadex-scout:9118/ab`,
  as two Torznab indexers (see
  [Per-tracker search gating](#per-tracker-search-gating)).
- **API Key:** the `indexer.feed_api_key` from step 1
- **Categories:** `5070` (Anime) in Sonarr, `2000` (Movies) in Radarr
- **☑ Anime Standard Format Search (required).** This is what makes Sonarr issue
  the whole-season query the feed answers.

**3. Create two Custom Formats.** Settings → Custom Formats → Add. Each gets a
single condition of type **Indexer Flag**:

| Custom Format | Condition (Indexer Flag) | Matches | From `downloadvolumefactor` |
| --- | --- | --- | --- |
| `SeaDex (best)` | `Freeleech25` | SeaDex's best pick | `0.75` |
| `SeaDex (alt)` | `Freeleech75` | a SeaDex-listed alt | `0.25` |

(Leave the condition's "negate" and "required" boxes unchecked. The flag names
are exactly `Freeleech25` and `Freeleech75`; the arrs derive them from the
feed's `downloadvolumefactor`.)

**4. Score them on your anime quality profile, and _only_ that profile.**
Settings → Profiles → your anime profile → Custom Formats: give `SeaDex (best)`
a high positive score (e.g. `100`) and `SeaDex (alt)` a lower one (e.g. `50`).
Sonarr/Radarr now prefer, and upgrade to, SeaDex's pick. Scoping the scores to
the anime profile keeps the markers from colliding with genuine AnimeBytes
Freeleech25/75 releases in your non-anime libraries.

### Per-tracker search gating

You may want a tracker used only for some search types: Nyaa (public) on
manual searches only, AnimeBytes on everything. Only the arr can enforce that
(a Torznab request never says which search type fired it), so the feed serves
each tracker on its own path (`…/nyaa`, `…/ab`) and the arr's per-indexer flags
decide: to make Nyaa manual-only, untick **Enable RSS** and **Enable Automatic
Search** on the Nyaa indexer (Settings → Indexers) and leave the AB one fully
enabled.

### Security

The feed is gated by `feed_api_key` (a request without the matching `apikey` gets
`401`). Its links are Prowlarr proxy URLs (for searches) and, for the AnimeBytes
RSS feed, direct AnimeBytes links that embed your `ab_passkey`, so treat the
endpoint as sensitive. Keep it on your LAN (behind an internal reverse proxy is
fine); don't put it on the public internet. The Prowlarr API key is sent to
Prowlarr in a request header (never in a logged URL) and is never written to
the logs.

The synthesized feed is persisted between cycles as `/config/feed.json`,
written owner-only (0600). Current versions never embed your `ab_passkey` in it
(AB links are rebuilt from the configured passkey on load, so a rotated passkey
takes effect on the next load); a snapshot written by an older version may
embed one until the first rebuild scrubs it.

## How matching works

SeaDex keys everything on AniList IDs; Sonarr keys on TVDB, Radarr on TMDB/IMDb.
seadex-scout bridges them:

- **ID mapping.** The Fribb `anime-list-mini.json` dataset maps `anilist_id` to
  `type` (TV vs movie), `tvdb_id`, `themoviedb_id`, and `imdb_id`. It is fetched
  with a conditional GET each cycle and cached, so an unchanged multi-MB file is
  never re-downloaded. The `type` decides which arr and which ID field to use.
  One exception, because the id is unambiguous: a record with no `tvdb_id` that
  carries a `themoviedb_id` **movie** id resolves as a Radarr movie whatever its
  `type` says (upstream labels some films as OVA/special). An `imdb_id` never
  gets that treatment - TVDB reuses a film's IMDb id on the parent series.
- **Overrides.** Drop a `/config/overrides.json` (a JSON array of records keyed
  by `anilist_id`) beside the config to pin the entries Fribb misses; it is
  applied on top of Fribb (operator records win). Absent is fine. Fields per
  record: `anilist_id` (required), `type` (`movie` routes to Radarr, anything
  else to Sonarr), `tvdb_id`, `tmdb_movies` (array of ints), `imdb_ids` (array
  of strings), `season_tvdb`. These are NOT the upstream Fribb field names
  (`imdb_id`, `themoviedb_id`, `season`), which are ignored with a warning
  naming the key. An override **replaces** the whole mapping record for its
  `anilist_id` (no field-by-field merge), so restate every field the entry
  needs.
- **Title fallback.** When an entry maps through neither source, seadex-scout
  fetches its titles and format from AniList and attempts a conservative
  normalized title-plus-year match against the library (exact match, single
  candidate required; ambiguous matches are skipped, not guessed).

## Release classification and filters

Each SeaDex release and each library file is classified into one vocabulary:
release group, tracker (public vs private), resolution, codec (x265/x264),
dual-audio, and **kind** (`remux` / `encode` / `unknown`). Remux-vs-encode is
name- and notes-based, never a size or bitrate inference; an unclassifiable
release is `unknown` and never silently dropped. The comparison is
**group-centric** (an item is aligned when a recommended release group is
already present on it), and unknown group evidence proves nothing: it surfaces
as the informational `unverifiable` finding and the report's `unverified`
verdict, never a confident answer.

The filter keys (`filters.*`, `animebytes`, `arr_tags.*`; see
[Configuration reference](#configuration-reference)) shape the report/alert
engine only; the [indexer](#indexer-torznab-feed) feed applies none of them,
with one exception: `filters.exclude_tags` shapes all three surfaces.
The two content filters (`exclude_remux`, `require_dual_audio`) shape the
daemon's findings only (the report always lists SeaDex's raw best/alt picks),
while `exclude_specials` and `animebytes` shape both.

**SeaDex curation tags are not filtered by default.** SeaDex curators tag some
listed releases `Broken` or `Incomplete`. seadex-scout filters **nothing** on
those tags unless you say so: with the default empty `filters.exclude_tags`, a
`Broken`-tagged release is alerted on as a better release available, counts as a
best in the report, and is served in the Torznab feed. The report annotates it
`(broken)` regardless, so the warning is always visible — it just is not acted
on. To exclude such releases, name the tag and the surfaces it should disappear
from:

```yaml
filters:
  exclude_tags:
    broken: [findings, report, feed]
    incomplete: [feed]
```

The surfaces are exactly `findings` (the daemon's alerts), `report`, and `feed`
(Torznab search + RSS), so a tag can be kept out of the feed while still being
alerted on. Tag matching is exact and case-insensitive (never a substring), any
SeaDex tag is filterable, and an unknown surface name — or a tag listing no
surfaces — is rejected at startup.

## Configuration reference

All configuration lives in one YAML file, `/config/config.yaml` (override the
path with `CONFIG_PATH`). On first boot with no config, seadex-scout writes a
commented starter there and exits with a warning; edit it and restart. The full
annotated template is [`config.example.yaml`](config.example.yaml). Any string
value may reference `SONARR_*`, `RADARR_*`, or `SEADEX_SCOUT_*` environment
variables with `${VAR}`, so secrets can stay out of the file; API keys are
never logged. At least one arr must be enabled with a `url` + `api_key`, and an
unknown or misplaced key is rejected at startup with an error naming it.

| Key | Default | Description |
| --- | --- | --- |
| `sonarr.enabled` / `radarr.enabled` | `false` | Enable that arr; at least one required |
| `sonarr.url` / `radarr.url` | `http://sonarr:8989` / `http://radarr:7878` | Arr base URL; used only when that arr is enabled |
| `sonarr.api_key` / `radarr.api_key` | _none_ | Arr API key; required when enabled |
| `sonarr.public_url` / `radarr.public_url` | _(unset)_ | Browser-facing base for report deep-links; falls back to `url` |
| `mode` | `daemon` | `daemon` (scheduled) or `report` (one-shot audit, then exit) |
| `poll_interval` | `15m` | Loop cadence for alerts + feed, clamped `15m`–`720h`. Most iterations are a cheap tick; every 24h worth is a full pass (see [Freshness](#freshness)). `off`/`disabled`/`0` = external trigger via `poll` — schedule that around 24h apart, since every `poll` is a full pass |
| `animebytes` | `false` | Include AnimeBytes releases and links (private tracker; links are member page URLs, no credentials needed) |
| `filters.exclude_remux` | `false` | Releases classified `remux` never count as a recommendation |
| `filters.require_dual_audio` | `false` | Only dual-audio releases count |
| `filters.exclude_specials` | `false` | Drop OVA/ONA/special entries from findings and the report |
| `filters.exclude_tags` | `{}` | Per-tag, per-surface SeaDex-tag exclusions (`findings`/`report`/`feed`); **empty = nothing is filtered, including `Broken`** |
| `filters.ignore` | `[]` | AniList IDs whose findings are never alerted on. Suppresses the **alert only** — the show still appears in report mode and the RSS feed is untouched. Use it for a show you have decided not to upgrade, so the reminders stop. Max 512 entries |
| `arr_tags.include` / `arr_tags.exclude` | `[]` | Scan only / never arr items with these tags; an exclude wins |
| `report.dir` | `/config/reports` | Where timestamped `report-<UTC>.md` + `.json` pairs land |
| `indexer.feed_api_key` | generated | Key the arrs must send to the feed; the first-boot starter generates one |
| `indexer.nyaa_torznab_url` / `indexer.ab_torznab_url` | `""` | Prowlarr per-indexer Torznab URLs; `""` disables that tracker, any set enables the feed |
| `indexer.prowlarr_api_key` | `""` | Prowlarr API key (secret, never logged) |
| `indexer.ab_passkey` | `""` | Builds the AB RSS feed's download links; `""` = AB RSS off (Nyaa needs none) |
| `log.level` / `log.format` | `info` / `json` | `debug`\|`info`\|`warn`\|`error`; `json`\|`text` |

The upstream endpoints (SeaDex, Fribb, AniList) and the internal state, override,
and feed-snapshot locations are fixed and not config keys. To pin a mapping
Fribb gets wrong, drop a `/config/overrides.json` beside the config (see
[How matching works](#how-matching-works)).

## Observability

Observability is slog-only: no metrics endpoint, and no HTTP surface unless you
configure the [indexer](#indexer-torznab-feed) feed, the only thing that binds a
port (fixed at `:9118`). An alert-only deployment stays socket-less.

- **slog to Loki.** A JSON handler writes to stdout; Alloy (or any collector)
  ships it to Loki. A finding is one line at `warn` (`msg="better release
  available"`) with `title`, `al_id`, `arr`, `season`, `current_group`,
  `recommended_group`, `tracker`, `resolution`, `kind`, `classification_reason`,
  a headline `release_url`, and `release_urls` (every obtainable source), plus
  the clickable per-source links (`arr_url`, `nyaa_url`, `ab_url` +
  `ab_tracker`, plus
  `public_url` + `public_tracker` when the public source is a tracker other than
  Nyaa) and
  `seadex_tags` an alert template can render directly.
  Informational cases (`incomplete`, `theoretical_best`, `mixed_group_manual`,
  `unverifiable`) log at `info`. Each cycle closes with a completion line:
  `cycle complete` when healthy, or `cycle degraded` at `warn` with a `reason`
  when a failed arr walk, an upstream outage, or a safety guard degraded the
  comparison; report mode emits one `report item` line per anime.
- **Alert rules.** seadex-scout ships no notifier of its own; see
  [Alerting](#alerting) for reference Loki ruler rules you can copy.
- **Health.** The distroless image's Docker `HEALTHCHECK` uses the
  `seadex-scout health` subcommand (a `/tmp/.healthy` file marker), so no shell or
  port is needed; the marker reflects the last cycle's library-ingest outcome.

## Freshness

Most loop iterations are a **tick**, not a full pass. A tick asks SeaDex one
~88-byte question — _what changed in the last 48 hours?_ — and does nothing else
when the answer is nothing, which is most of the time. When something did
change, it fetches just those entries (a few tens of KB), folds any new releases
into the RSS feed, and reports the findings they produce.

Every 24 hours' worth of iterations, and always on start, one iteration is a
**full pass** instead: the whole catalogue, a full Sonarr/Radarr walk, the whole
Torznab search index and feed rebuilt. That pass is the backstop for everything
a 48-hour window cannot see — a release removed from SeaDex, a de-curation, an
edit that did not bump its entry, a shared torrent's other entries, an outage
longer than the window, and a badly wrong container clock. It is also what
refreshes the library snapshot the ticks compare against.

What that buys, measured against the live catalogue: the feed and the alerts go
from up to 3 hours stale to about 15 minutes, while **upstream traffic drops by
roughly 85%** — because a timer-driven full pass spent most of its bandwidth
discovering that nothing had changed (77% of 3-hour windows contain no SeaDex
change at all).

Two consequences worth knowing:

- **An upgrade you perform is noticed by the next full pass, not the next
  tick.** A tick compares against the cached library snapshot, so a finding you
  have already acted on can keep being reported for up to a day. This is the
  deliberate trade for not walking your arrs every 15 minutes.
- **The search index is rebuilt daily, not every 15 minutes.** Search covers
  SeaDex's long tail, which does not move quickly; the RSS feed is what carries
  anything new. One visible consequence: for up to a day, an alert can name a
  release that a manual search in Sonarr/Radarr does not return yet. The release
  is real — it is already in the RSS feed, so the arrs can still grab it, and the
  next full pass puts it in the search index.

Both `SeadexScoutScanStalled` (the loop is alive) and
`SeadexScoutReconcileStalled` (the backstop still runs) are shipped in
[`alerts.yaml`](alerts.yaml) — the second matters because a healthy stream of
ticks would otherwise hide a full pass that had silently stopped.

**It costs log volume.** Findings are state, so every iteration re-emits the whole
set — at a 15-minute interval that is roughly ten times the log lines the old
3-hour cycle produced (order of 18k finding lines a day for a library with ~190
open findings). Upstream bandwidth drops by ~85%; part of what pays for that is
Loki ingestion and local disk writes. Worth checking your retention before
upgrading if it is tight.

## Alerting

seadex-scout ships no notifier of its own; its operational state is in its logs
(there is no metrics endpoint). Ship the container's logs to Loki (Grafana
Alloy's Docker log discovery does this with no configuration) and evaluate the
rules in [`alerts.yaml`](alerts.yaml) with
[Loki's ruler](https://grafana.com/docs/loki/latest/alert/); firing alerts
deliver through your Alertmanager like any Prometheus metric alert. They cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `SeadexScoutCycleError` | a cycle logs an error: the Sonarr/Radarr library walk failed, a state write failed, a queued rerun could not record poll health, or a persisted degradation streak escalated (library shrink, SeaDex fetch, partial walk and AniList lookups after 2 consecutive full reconciles; a rejected mapping refresh or an unreadable SeaDex after 8 ticks). Routine outcomes stay WARN and never fire it (see `alerts.yaml`) | warning |
| `SeadexScoutScanStalled` | no sign of life in 3h — the daemon emits `tick`/`cycle` `complete` or `degraded` on every iteration and `reconcile started` when a full pass begins, so the absence of all of them means the poll loop is wedged | warning |
| `SeadexScoutReconcileStalled` | no `reconcile complete` line in 72h, i.e. the daily full pass has silently stopped while ticks keep the loop looking alive | warning |
| `SeadexScoutBetterReleaseFound` | SeaDex recommends a better release than the one on disk (informational, not a fault). **A state signal, not an event** — see below | info |
| `SeadexScoutReportWritten` | a report run wrote a season-level alignment report (informational) | info |

**Findings are reported as state, not as events.** Every loop iteration re-emits
every finding that is currently true — including an iteration that found nothing
to do or could not reach SeaDex, since neither is evidence that a standing
finding was resolved — so `SeadexScoutBetterReleaseFound` keeps firing
until you upgrade the release (or add the show to `filters.ignore`). That is
what makes a notification lost between the app and your receiver recoverable:
the condition is still being reported, so the next rule evaluation fires it
again. Two consequences to plan for:

- **The rule's lookback window must exceed your `poll_interval`, with margin.**
  Loki's ruler does not support `keep_firing_for`, so the window is the only
  thing holding an alert firing between emissions. The shipped `[12h]` tolerates
  a long run of missed iterations at the 15m default. Set it too tight and every
  quiet stretch produces a fire → resolve → re-fire flap.
- **Your Alertmanager route decides how often you are reminded.** A finding that
  stays true is re-notified every `repeat_interval`, so that value — not the app —
  is what makes an open recommendation nag or stay quiet. These are
  announcements rather than issues, so the shape that reads best is one
  notification per recommendation and effectively no repeat: group on the
  finding's identity (`al_id`, `season`, `alert_recommended_group`, `info_hash`)
  and set a long `repeat_interval`. Keep
  `send_resolved: false` — a "resolved" message when you finally download a
  release tells you something you already know. `filters.ignore` is how you stop
  a show you have consciously declined from being announced at all.
- **A long `repeat_interval` alone does not stop the repeats, and this one bites
  silently.** Alertmanager's notification log is what remembers that a group was
  already notified, and it expires on `--data.retention` (default **120h**, five
  days). Once the entry is gone the still-firing alert is treated as new and
  notified again — so the effective repeat is whichever of `repeat_interval` and
  the retention is _smaller_, and a one-year interval on a default install
  re-announces everything every five days. Raise the retention alongside the
  interval and keep the interval under it. On Prometheus Alertmanager that is
  `--data.retention`; on Grafana Mimir's built-in Alertmanager it is
  `-alertmanager.storage.retention` (same 120h default). Alertmanager logs a
  warning when `repeat_interval` exceeds retention — worth reading the startup
  logs once after changing either. Upstream states the rule in
  [prometheus/alertmanager#2890](https://github.com/prometheus/alertmanager/issues/2890).
- **One consequence of not repeating, stated plainly.** The repeat is also the
  redelivery: if Discord is unreachable when an announcement fires and stays
  unreachable past Alertmanager's own retries, that announcement is gone rather
  than retried later. It is an acceptable loss here because the notification is
  not the automation path — the release is already in the Torznab feed, so
  Sonarr/Radarr see it regardless, and the finding stays in the logs and the
  report. If you would rather have a safety net, a monthly `repeat_interval`
  reads as "never" to a human while still giving a failed send another chance —
  and note that a month is already past the 120h default, so it needs the
  retention raised too.

**Re-copy `alerts.yaml` when you upgrade to this version.** The rules changed
shape with the two cadences: the stall deadman now matches the tick's completion
lines and the full pass's start line over a 3h window, and
`SeadexScoutReconcileStalled` is new. A previously deployed copy that matches
only `cycle (complete|degraded)` sees one line a day and false-fires for most of
every day.

Upgrading an existing install re-announces every open finding once, because the
new model reports what is true rather than what is new. Every one of those lines
is accurate; put the ones you have already declined in `filters.ignore`.

Thresholds and the `severity` labels are starting points; adjust the
`container` selector and the stall window to your deployment and
`poll_interval`. In resident-idle (`poll_interval: off`) or report mode each
cycle runs via a `docker exec` child, so its logs go to the trigger rather than
the container's log stream. That blinds the count-based rules (they can never
see their lines and never fire) but does **not** silence the deadman:
`SeadexScoutScanStalled` watches for the _absence_ of completion lines, which
now never reach the stream, so it **false-fires** once its window elapses. In
external mode, drop the deadman (or retarget its selector at the stream that
does carry the completion lines) and alert on your scheduler's own job result
instead. The rules assume the default JSON log handler; for
`log.format: text`, swap the `| json | level="ERROR"` parser stage for a
`|= "level=ERROR"` line filter.

## Contributing

Issues and PRs are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
conventions and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0. Linking [`arrapi`](https://github.com/cplieger/arrapi) (GPL-3.0) makes
seadex-scout GPL-3.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

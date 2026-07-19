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
   (remux policy, AnimeBytes on/off, dual-audio).
4. Compares the surviving recommendation against what you have and, when SeaDex
   has something better, emits a `warn` log line.

When the [Torznab feed](#indexer-torznab-feed) is configured, the same cycle also
rebuilds it from that one SeaDex fetch, so a finding and what the arrs can grab
from the feed always reflect the same refresh.

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

- **Built-in** (default): `poll_interval` is a Go duration (`3h` default, `6h`;
  minimum `1h`, shorter values are clamped up to `1h`); a cycle runs on start,
  then every interval. It is the single cadence for
  both the alert loop and the Torznab feed.
- **External / resident-idle**: set `poll_interval: off` (or `disabled` / `0`).
  There is no internal timer; the container idles healthy and an external
  scheduler drives each cycle via the `poll` subcommand — which runs one cycle,
  updates the health marker, and exits `0`/`1`. Concurrent cycle requests
  coalesce on a cross-process cycle lock (`cycle.lock` in `/config`): a `poll`
  arriving while another cycle is in flight queues one rerun for the active
  runner (extras are discarded — a queued rerun already guarantees a fresh
  run) and exits `0` immediately, and in built-in mode a timer tick that lands
  mid-cycle is skipped with a warning. The Torznab feed, when configured,
  refreshes on that same `poll` (it is served from the last cycle's snapshot, so it
  is empty until the first `poll` runs). With
  [Ofelia](https://github.com/mcuadros/ofelia), label the service:

  ```yaml
      labels:
        ofelia.enabled: "true"
        ofelia.job-exec.seadex-poll.schedule: "@every 3h"
        ofelia.job-exec.seadex-poll.command: "/seadex-scout poll"
  ```

  Any scheduler works — `docker exec seadex-scout /seadex-scout poll` is the whole
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
- `unverified`: the release-group evidence is unknown on one side — an on-disk
  file with no identifiable group, or a SeaDex release with no group tag (both
  carried as the `NOGRP` placeholder) — so alignment could be neither proven
  nor ruled out. Absence of evidence is never read as evidence: such a row is
  a manual check, not a `have_best` or a `have_unlisted`.

A listed release you could not act on — no usable link, or a tracker you
cannot use — stays visible in the row but never drives the verdict: the
Markdown annotates it (`PMR (unobtainable)`, like a curation-warned release's
warning tags) and the JSON marks it `"unobtainable": true`, so a visible best
the verdict ignored is always explained.

After the per-match verdicts, a trailing **`not_on_seadex`** section lists library
items recognized as anime (through the Fribb catalogue) that SeaDex does not list
at all, so you can see which of your titles SeaDex has not curated.

Every row carries three kinds of link: the Sonarr/Radarr deep-link to the item
(Sonarr via its title slug, Radarr via its TMDB id), the SeaDex entry
(`releases.moe/{alID}`), and the indexer link for each best release. The report
is written three ways, so it is both human- and machine-readable: a Markdown file
grouped by verdict, a JSON file alongside it, and one `report item` slog line per
anime (queryable in Loki like the daemon findings when the report runs as the
container command — `mode: report` or the `report` container arg; a
`docker exec` run's output goes to the exec session, not the container log
stream, so Alloy/Loki never see it — Ofelia captures job-exec output in its
own logs instead). Each run writes a timestamped
`report-<UTC date+time>.md` + `.json` pair into `report.dir` (default
`/config/reports`) — e.g. `report-2026-07-11T15-04-05Z.md` — so successive
reports never overwrite one another (two reports generated within the same
UTC second get a deterministic `-2`/`-3`/... suffix on the later pair).
Reports are never deleted by the app; if
you schedule them, prune old pairs yourself. Run
`find /config/reports -name 'report-*' -mtime +90 -delete` from a helper container that
has `find` and mounts the same config volume at `/config`, or use host cron against the
host-side `./config/reports` path. The seadex-scout image is distroless and has no `find`.

> When you run `report` as the container's command (rather than `docker exec`
> into the running daemon), disable the image's baked healthcheck for that
> one-shot container (compose: `healthcheck: { disable: true }`; docker run:
> `--no-healthcheck`). The health marker belongs to the daemon's poll loop, so
> a report-only container would read unhealthy while the report is still
> generating, and an unhealthy-restart watchdog could kill it mid-run. And give
> it no restart policy (`restart: "no"`): `mode: report` is a one-shot that
> exits when the report is written, so a restart-policied container loops it —
> every restart runs another full library audit and writes another timestamped
> `report-<UTC>.md`+`.json` pair, indefinitely.

While the daemon runs, produce a fresh report without stopping it by running the
`report` subcommand in the container:

```sh
docker exec seadex-scout /seadex-scout report
```

The `.md` and `.json` land in `report.dir` on the `/config` volume, where you
read them directly. A report never writes the state cache, so it is safe to run
alongside a daemon cycle. To produce reports on a schedule, use the same
Ofelia `job-exec` pattern as above with `/seadex-scout report`.

Two details of the write are worth knowing. Report runs are serialized through
a `report.lock` file in `report.dir`: a second run started while one is in
flight refuses with `another report is already running` and exits `1`, rather
than racing the first onto the same timestamped filenames. And each pair is
written JSON first, so a run interrupted mid-write can leave a `.json` without
its `.md`, but never a Markdown file without its machine-readable pair.

Reports are written owner-only (`0600` files, newly created report directories
`0700`): they enumerate your library and can carry private-tracker page links,
so other local accounts able to traverse the bind-mounted config tree must not
read them. Writes never retighten what already exists on disk, so if you
generated reports with an older version, tighten the historical pairs and
directory once with `chmod -R go-rwx /config/reports` — run it against the
host-side path or from a helper container mounting the same volume, like the
pruning command above (the distroless image has no shell).

## Indexer (Torznab feed)

When a Prowlarr Torznab URL is configured, the daemon serves a
[Torznab](https://torznab.github.io/) feed of SeaDex releases for Sonarr/Radarr,
alongside the compare loop in the same process. It is the opt-in automation path:
unlike the report-only findings, it lets the arrs grab. Point your arrs at it
(directly or through Prowlarr) and they parse, match, and grab through their own
engines, profiles, and history, exactly as for any other indexer.

The feed handles its two request kinds two different ways. A **search** (the arr's
automatic or interactive search, which carries a query) is **proxied to Prowlarr**:
it asks Prowlarr's Nyaa and AnimeBytes Torznab endpoints, keeps only the results
SeaDex curates, and passes their real title, seeders, size, and Prowlarr-proxied
download link straight through — so a search needs no tracker passkey here
(Prowlarr grabs with the AnimeBytes credentials it holds). If every upstream
query fails (Prowlarr unreachable), the search answers with a Torznab
`<error code="900">` document rather than an empty feed, so the arr records a
failed search instead of concluding there were no results. A **periodic RSS check**
(the no-query "recent releases" fetch the arrs run on their sync interval) is
served from an **incremental journal of newly curated releases**: a release
appears in the RSS feed when it is _new to SeaDex's curation_ — present in the
current curation set, absent from every set seen before (the tracker's post
date is deliberately not the trigger: SeaDex routinely curates old torrents) —
and stays listed for 14 days before aging out, plenty for RSS polls that run on
a minutes-scale interval and enough to ride out a week-long arr outage. On the
very first cycle (and after this schema's upgrade) the whole current curation
set is recorded as already-seen and the RSS feed starts **empty**, growing only
as SeaDex curates new releases — catching up an existing library is what
searches and [the report](#the-report) are for. The flip side: a release
curated before the journal began, or one that has aged past the 14-day window,
surfaces **only** through a search. A deployment that relies on RSS alone — or
one that leaves **Anime Standard Format Search** off, so the feed never answers
Sonarr's queries — quietly misses that long tail. Each journal item carries a
download link built directly: a public Nyaa `.torrent`, or an AnimeBytes link
built from your `ab_passkey` (see below).

Journal items get an **arr-parseable title built from real metadata**, not from
SeaDex's raw file names alone: the show's own title as your arr knows it (from
the last library snapshot; the arr is guaranteed to parse its own title back),
falling back to the AniList canonical title, and only then to a file-name
derivation. A season pack is labeled with its mapped TVDB season (or the
dominant real season across the pack's files, so a pack bundling S00 specials
with S01 episodes reads S01), a single episode keeps its own `SxxExx`/absolute
marker, a movie reads `Title (Year)`, and the flags the app actually holds are
suffixed — resolution, `Dual Audio`, the release group bracketed; nothing is
invented. On top of that the feed **harvests real release titles**: each cycle
it spends up to 15 Prowlarr queries (one per show, series-level on AnimeBytes,
season-form with offset paging on Nyaa) matching curated torrents by tracker id
or info hash, and caches each matched title permanently — so items upgrade from
a synthesized title to the tracker's real one, usually within a cycle or two,
while the synthesized title remains a fully working fallback. GUIDs never
change with a title upgrade, so the arrs never re-grab.

One rendering caveat: a synthesized RSS item always reports `seeders=1` — the
feed has no live swarm data, and the floor keeps the arrs' minimum-seeders
check from rejecting a curated release outright. That means a torrent that is
actually dead still looks grabbable in the RSS feed, and a grab from it can sit
stalled in the download queue. Minimum-seeder rejection only has real data to
work with on **search** results, which pass the tracker's live seeder counts
through — subject to the same floor: a live count of 0 also renders as 1, so
only a minimum-seeders setting above 1 can reject a dead search result.

Every item, either way, carries a **download-volume-factor marker**: SeaDex's
_best_ release is tagged `0.75` (which the arrs read as AnimeBytes Freeleech25) and
an _alt_ `0.25` (Freeleech75) — the signal you map to a Custom Format (below). Each
item's category is the entry's real media type, resolved from the anime-list
mapping rather than guessed from the file name: a film is `2000` (Movies → Radarr),
while a series, OVA, or special is `5070` (Anime → Sonarr), so a single-file
special is never mistaken for a movie.

**It answers whole-season searches, not per-episode ones.** SeaDex tracks season
packs, so the feed answers a season search with the pack and returns nothing —
without contacting a tracker — for a per-episode query. That spares the trackers a
query per episode and makes a manual single-episode search free; specials and
movies are single releases and are always answered.

> **Setup requirement:** because the feed relies on the season search, enable
> **Anime Standard Format Search** on the seadex-scout indexer in Sonarr (Settings
> → Indexers → the indexer). Without it, Sonarr sends only per-episode queries,
> which the feed does not answer.

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
  feed_api_key: "a-random-string"                   # generate with: openssl rand -hex 16
  nyaa_torznab_url: "http://prowlarr:9696/1/api"    # "" disables Nyaa
  ab_torznab_url: "http://prowlarr:9696/2/api"      # "" disables AnimeBytes
  prowlarr_api_key: "${SEADEX_SCOUT_PROWLARR_KEY}"  # secret, never logged
  ab_passkey: "${SEADEX_SCOUT_AB_PASSKEY}"          # AnimeBytes passkey; required for the AB RSS feed, "" leaves it off
```

The port is fixed at `:9118` (published by your compose port mapping, not a config key).

Leave the **Nyaa** indexer's **Sort requested from site** (under its advanced
settings in Prowlarr) at its default (created/date, descending): the feed's
title harvest pages through results by offset under that ordering to reach
older curated items, and a different sort would reshuffle the pages it walks.

For **searches**, the download links are Prowlarr's own proxy URLs, so no passkey
is needed — Prowlarr grabs with the credentials it holds. The **AnimeBytes RSS
feed** is the one exception: SeaDex never publishes AB download links, so the feed
builds them from your `ab_passkey` (the token in your AnimeBytes RSS/announce URL).
Leave it empty and the `/ab` feed has nothing grabbable to serve, so it returns a
clear error and Prowlarr's save-test fails until you set it — Nyaa is public and
needs nothing. The passkey rides in the AB feed's links, so keep the endpoint on
your LAN (see [Security](#security)).

**2. Add the feed to Sonarr/Radarr.** Settings → Indexers → Add → **Torznab**
(Custom):

- **URL:** the feed is **per-tracker** — add Nyaa as
  `http://seadex-scout:9118/nyaa` and AnimeBytes as `http://seadex-scout:9118/ab`,
  as two Torznab indexers. There is no combined endpoint; serving each tracker on
  its own path is what lets you gate their search types independently (see
  [Per-tracker search gating](#per-tracker-search-gating)).
- **API Key:** the `indexer.feed_api_key` from step 1
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

### Per-tracker search gating

You may want a tracker used only for some search types — e.g. Nyaa (public) on
manual searches only, AnimeBytes on everything. The arr already enforces
per-indexer **Enable RSS / Enable Automatic Search / Enable Interactive Search**
flags, and it is the only component that can: the search _type_ is never carried
in a Torznab request (only RSS, the no-query "recent" feed, is distinguishable —
confirmed in Sonarr's `NewznabRequestGenerator`). So the feed exposes each tracker
on its own path and lets the arr decide when to hit each:

| Feed | Path | Or subdomain |
| --- | --- | --- |
| Nyaa | `…/nyaa` | `nyaa.example.com` |
| AnimeBytes | `…/ab` | `ab.example.com` |

Add the per-tracker feeds as **two** Torznab indexers (each still needs Anime
Standard Format Search on), then set their flags in Sonarr/Radarr (Settings →
Indexers): to make Nyaa manual-only, untick **Enable RSS** and **Enable Automatic
Search** on the Nyaa indexer and leave the AB one fully enabled. Adding them
through Prowlarr with a sync profile works too — the flags just have to end up on
the indexer as the arr sees it.

If seadex-scout runs apart from the arrs behind a reverse proxy, the subdomain
form is cleaner than the path: point `nyaa.example.com` and `ab.example.com` at
the one `:9118` and the feed picks the tracker from the hostname — no path rewrite
and no second port. The proxy must pass the `Host` header through unchanged (the
default for Caddy/nginx `reverse_proxy`).

### Security

The feed is gated by `feed_api_key` (a request without the matching `apikey` gets
`401`). Its links are Prowlarr proxy URLs (for searches) and, for the AnimeBytes
RSS feed, direct AnimeBytes links that embed your `ab_passkey` — so treat the
endpoint as sensitive: keep it on your LAN — behind an internal reverse proxy is
fine (that is what the per-tracker subdomain routing is for), just don't put it on
the public internet. The Prowlarr API key is sent to Prowlarr in a request header
(never in a logged URL) and is never written to the logs.

The synthesized feed is also persisted on disk between cycles as
`/config/feed.json`. AnimeBytes items are stored GUID-only — the persisted
snapshot never embeds your `ab_passkey`; the served AB download links are
derived from the currently configured passkey each time the snapshot is
loaded (which is also why a rotated passkey takes effect on the next load).
The file is still written owner-only (0600) as defense in depth, and a
snapshot written by a pre-journal version may embed a passkey until the
first rebuild scrubs it.

## How matching works

SeaDex keys everything on AniList IDs; Sonarr keys on TVDB, Radarr on TMDB/IMDb.
seadex-scout bridges them:

- **ID mapping.** The Fribb `anime-list-mini.json` dataset maps `anilist_id` to
  `type` (TV vs movie), `tvdb_id`, `themoviedb_id`, and `imdb_id`. It is fetched
  with a conditional GET each cycle and cached, so an unchanged multi-MB file is a
  cheap `304` and is never re-downloaded. The `type` decides which arr and which ID field
  to use.
- **Overrides.** Drop a `/config/overrides.json` (a JSON array of records keyed
  by `anilist_id`) beside the config to pin the entries Fribb misses; it is
  applied on top of Fribb (operator records win). Absent is fine. Fields per record: `anilist_id`
  (required), `type` (`movie` routes to Radarr, anything else to Sonarr),
  `tvdb_id`, `tmdb_movies` (array of ints), `imdb_ids` (array of strings),
  `season_tvdb` — note these are NOT the upstream Fribb field names
  (`imdb_id`, `themoviedb_id`, `season`), which are ignored with a warning
  naming the key whenever the mapping is loaded. An
  override **replaces** the whole mapping record for its `anilist_id` (no
  field-by-field merge), so when correcting an entry Fribb already has,
  restate every field the entry needs.
- **Title fallback.** When an entry maps through neither source — and also when
  Fribb _has_ the entry but its record carries no arr identifier the entry's
  type routes to (an id-less record: no TVDB id for a series, no TMDB/IMDb
  movie id for a film) — seadex-scout fetches its titles and format from
  AniList and attempts a conservative normalized title-plus-year match against
  the library (exact match, single candidate required; ambiguous matches are
  skipped, not guessed). A record that does carry its arr id but simply isn't
  in the library is not title-matched, so mapped items never hit AniList and
  steady-state AniList traffic is near zero.

## Release classification and filters

Each SeaDex release and each library file is classified into one vocabulary:
release group, tracker (public like Nyaa vs private like AnimeBytes),
resolution, codec (x265/x264), dual-audio, and **kind** (`remux` / `encode` /
`unknown`). The remux-vs-encode decision is name- and notes-based, never a size
or bitrate inference; an unclassifiable release is `unknown` and is never
silently dropped.

The comparison is **group-centric**: an item is aligned when a recommended
release group is already present on it. This sidesteps most multi-cour and
batch-vs-per-season breakage. Group evidence parsed from release names is
three-valued — known group, known different group, or unknown (a group-less
file or an untagged SeaDex release) — and unknown evidence proves nothing:
instead of a confident verdict, such a comparison surfaces as the
informational `unverifiable` finding status and the report's `unverified`
verdict.

These filters shape the **report/alert engine only** — the
[indexer](#indexer-torznab-feed) feed applies none of them; there the arrs
filter through their own quality profile and Custom Formats. Their scope
differs by engine: the two content filters (`exclude_remux`,
`require_dual_audio`) shape the daemon's findings only — the report always
lists SeaDex's raw best/alt picks — while `exclude_specials` and the
`animebytes` toggle shape both the findings and the report. All are optional:

- `filters.exclude_remux` (default false): when true, releases classified
  `remux` never count as a recommendation. The default keeps them — on SeaDex a
  remux is often the best release — and `unknown` is never auto-dropped.
- `filters.require_dual_audio` (default false): drop releases that are not
  dual-audio.
- `filters.exclude_specials` (default false): when true, drop OVA/ONA/special
  entries from findings and the report.
- `animebytes` (default false; top-level, since it is tracker access rather than
  a content filter) is the one tracker knob. SeaDex lists releases on just two
  trackers: public **Nyaa** and private **AnimeBytes**. The public trackers
  (Nyaa, AnimeTosho, RuTracker) are always considered; AnimeBytes is included
  only when you turn this on, i.e. you have an account. Off, AB releases are
  invisible; on, a finding carries every source, so a release on both Nyaa and AB
  shows both links. Because seadex-scout only links (never downloads), an AB link
  is just the torrent page you open as a member: no tracker API keys or
  credentials are needed.
- `arr_tags.include` / `arr_tags.exclude` (arr-side): scan only items carrying an
  include tag, and never items carrying an exclude tag — an exclude wins when an
  item has both.

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
poll_interval: "3h"               # cadence for BOTH alerts + Torznab feed; off/disabled/0 = external trigger via `poll`

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

At least one arr must be `enabled` with a `url` + `api_key`; an enabled arr
missing either is a configuration error. Unknown or misplaced keys are also
rejected at startup with an error naming the key (e.g. `unknown configuration
key "anime_bytes"`), so a typo fails fast instead of being silently ignored.
`public_url` is the browser-facing base
used only for the report's deep-links (leave empty to reuse `url`), so an
internal Docker hostname in `url` still yields working links.

The upstream endpoints (SeaDex, Fribb, AniList), their request cadences, and the
internal state, override, and feed-snapshot locations are fixed under `/config`
and are not config keys. Report output is the exception: `report.dir` selects
its destination (default `/config/reports`). To pin a mapping Fribb gets wrong,
drop a `/config/overrides.json` beside the config (see [How matching works](#how-matching-works)).

## Observability

Observability is slog-only: no metrics endpoint, and no HTTP surface unless you
configure the [indexer](#indexer-torznab-feed) feed — the only thing that binds a
port (fixed at `:9118`). An alert-only deployment stays socket-less.

- **slog to Loki.** A JSON handler writes to stdout; Alloy (or any collector)
  ships it to Loki. A finding is one line at `warn` (`msg="better release
  available"`) with `title`, `al_id`, `arr`, `current_group`,
  `recommended_group`, `tracker`, `resolution`, `kind`, `classification_reason`,
  a headline `release_url`, and `release_urls` (every obtainable source, so a
  release on both Nyaa and AnimeBytes carries both). Informational cases
  (`incomplete`, `theoretical_best`, `mixed_group_manual`, and `unverifiable` —
  the release-group evidence on one side is unknown, so neither alignment nor
  a better release can honestly be claimed) log at `info`. Each
  cycle closes with a completion line: `cycle complete` (carrying the counts,
  mapping coverage, and AniList usage) when healthy, or `cycle degraded` at
  `warn` with a `reason` when an upstream outage or a safety guard skipped or
  degraded the comparison (a partial walk or a stale-but-usable ID map still
  compares, but the cycle closes degraded); report mode emits one
  `report item` line per anime.
- **Alert rules.** seadex-scout ships no notifier of its own; see
  [Alerting](#alerting) for reference Loki ruler rules you can copy (a cycle
  fault, a poll-loop deadman, and informational better-release and
  report-written rules).
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
| `SeadexScoutCycleError` | a cycle logs an error: the Sonarr/Radarr library walk failed, or a library-shrink / mapping-refresh guard escalated after 8 consecutive degraded cycles | warning |
| `SeadexScoutScanStalled` | no cycle completion line (`cycle complete` or `cycle degraded`) in 7h, i.e. the daemon poll loop is wedged | warning |
| `SeadexScoutBetterReleaseFound` | SeaDex recommended a better release than the one on disk (informational, not a fault) | info |
| `SeadexScoutReportWritten` | a report run wrote a season-level alignment report (informational) | info |

Thresholds and the `severity` labels are starting points. Adjust the `container`
selector (or `job` / `service`, depending on your log collector) to your
deployment and the stall window to your `poll_interval` (default 3h). In
resident-idle (`poll_interval: off`) or report mode each cycle runs via a
`docker exec` child (the `poll` / `report` subcommand), so its logs go to the
trigger rather than the container's log stream. That blinds the count-based
rules (`SeadexScoutCycleError`, `SeadexScoutBetterReleaseFound`,
`SeadexScoutReportWritten` — they can never see their lines and never fire),
but it does **not** silence the deadman: `SeadexScoutScanStalled` watches for
the _absence_ of cycle-completion lines, which now never reach the stream, so
it **false-fires** once its window (7h + 1h `for`) elapses. When running
external mode, drop the deadman or retarget its selector at the stream that
does carry the completion lines (your runner's log stream, if the collector
ships it), and alert on your external scheduler's own job result instead. The
rules assume the default JSON log handler; for `log.format: text`, swap the
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
[indexer](#indexer-torznab-feed) feed, which serves on a fixed `:9118`; publish
that port only on your LAN.

## Report-only, on purpose

The monitoring and report engines never grab — they surface an actionable finding
with the SeaDex and tracker links, and you decide. SeaDex recommendations are
curation judgments that interact with storage budget, tracker access, and per-show
taste, so a human-in-the-loop nudge beats a blind auto-grab. The opt-in
[indexer](#indexer-torznab-feed) is the middle path: it hands releases to
Sonarr/Radarr and lets them grab through their own engine, so nothing bypasses the
tooling you already trust.

## License

GPL-3.0. Linking [`arrapi`](https://github.com/cplieger/arrapi) (GPL-3.0) makes
seadex-scout GPL-3.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

# How seadex-scout works

This page explains how seadex-scout checks your library, matches shows to SeaDex, builds the report and serves the indexer. Read it when you want to know why a show got a certain result.

## Checks and full passes

seadex-scout runs one full pass when it starts and every 24 hours after that. A full pass has four steps:

1. It reads the Sonarr and Radarr anime library, honouring the `arr_tags` include and exclude lists, and records each item's current release. That record holds the release group, resolution, codec, whether it is a remux or an encode, and whether it has dual audio.
2. It matches each SeaDex entry to a library item by AniList ID, through the [animap](https://github.com/cplieger/animap) ID map. Entries that do not map fall back to an AniList title match.
3. It filters SeaDex's recommended releases by your settings for remuxes, AnimeBytes and dual audio.
4. It compares the recommendation that is left with what you have, and logs a `warn` line when SeaDex has something better.

Between two full passes, a quick check runs every `poll_interval`, 15 minutes by default. It asks SeaDex what changed in the last 48 hours and compares only those entries against the library the last full pass read. The load on SeaDex therefore follows how often SeaDex changes, not how often you check. Quick checks keep the findings and the indexer fresh within minutes. The full pass catches what a 48-hour window cannot see, such as a release SeaDex removed. The 24-hour cadence is fixed and is not a setting.

When the indexer is configured, the same pass rebuilds it from the same SeaDex data. A log finding and what Sonarr and Radarr can grab from the indexer always come from the same refresh.

## The report

For every anime with a SeaDex match, the report says which release you have. It also says whether that is SeaDex's best, a listed alternative, or neither.

The report works per season. Each SeaDex entry covers one AniList ID, which is one cour, movie or special. seadex-scout places the entry in its TVDB season through the animap ID map. It then compares the entry with the groups on disk for that season. Each row gets one of these verdicts:

| Verdict | Meaning |
| --- | --- |
| `have_best` | You have a release SeaDex marks best. |
| `have_alt` | You have a listed alternative, and SeaDex marks a different release best. |
| `have_older_revision` | You have SeaDex's best group, but an older revision, such as v1 against SeaDex's v2. The Scope cell shows both, as `revision v1, SeaDex v2`. |
| `have_unlisted` | You have a release SeaDex does not list. |
| `no_file` | Season not found. No files sit where the entry maps, because they are missing or Sonarr files that season under specials. |
| `unverified` | Files are present, but one side has no release group, or the files could not be read. Two untagged sides count as a match. |
| `unattributed` | Unmapped specials. A film or special in Sonarr's season 0 that no file is tied to. The indexer offers it, and your profile decides. |

A trailing `not_on_seadex` section lists the library items recognized as anime, through the animap ID map, that no comparable SeaDex entry covers. It shows which of your titles have no recommendation to compare against. That includes an item whose only SeaDex entries are films or specials the app offers without comparing. A row there means nothing comparable covers those files, which is not always the same as SeaDex never having heard of the show. A `not_on_seadex` row links only the library item, because it has no comparable SeaDex entry.

Every other row links the Sonarr or Radarr item, the SeaDex entry, and each best release.

Each run writes a timestamped pair into `report.dir`, `/config/reports` by default. `report-<UTC date+time>.md` is grouped by verdict, and `report-<UTC date+time>.json` sits beside it. The run also logs one `report item` line per anime. Runs never overwrite one another, and the app deletes no reports, so remove old pairs yourself. Each file is readable only by its owner, with mode `0600`.

Before it reads the library, a report checks that it can write such a file into `report.dir`. If it cannot, the run logs `seadex-scout failed` with the cause and exits with `1`. The usual causes are a read-only or full mount, a folder the container user cannot write, or an inherited ACL that widens the mode.

The report shows SeaDex's best and alternative releases as they are, without the remux and dual-audio filters. It does follow `animebytes`, `filters.exclude_specials` and the `report` entries of `filters.exclude_tags`.

A report never writes the saved state, so it is safe to run beside a running check. The output of a `docker exec` report goes to that exec session, not to the container log, so a log collector never sees its `report item` lines.

When you run `report` as the container's own command instead of through `docker exec`, turn off the image's healthcheck for that one-off container. In compose that is `healthcheck: { disable: true }`, and with `docker run` it is `--no-healthcheck`. The health marker belongs to the daemon's checks, so a report-only container reads unhealthy while the report is still running. A watchdog that restarts unhealthy containers could then stop it halfway.

## Matching

SeaDex keys everything on AniList IDs. Sonarr keys on TVDB IDs, and Radarr on TMDB and IMDb IDs. seadex-scout bridges them in four ways.

### ID mapping

The [animap](https://github.com/cplieger/animap) file `animap.json` maps each `anilist_id` to a `type`, such as TV or movie, and to a TVDB ID, TMDB movie IDs and IMDb IDs. The `type` decides which app is tried first. A movie is looked up by TMDB movie ID, then IMDb ID, in Radarr. Anything else is looked up by TVDB ID in Sonarr. A film you do not have in Radarr then falls back to its TVDB ID, which is the series TVDB files the film under, so it links in Sonarr instead of being lost.

Every full pass and every quick check that downloads SeaDex changes asks GitHub whether a new `animap.json` was released, and downloads the file only when it changed. A quick check that downloads none does not ask, so seadex-scout picks up a new release within 24 hours at most. The last good copy is kept in `state.json`, so seadex-scout keeps matching through a GitHub outage. A new file that fails its safety checks, such as one that lost most of its records, is refused and the last good copy stays in use.

### Episode mapping

Each record's mapping list in `animap.json` adds two facts. A record finds its list by its AniDB ID. A special that AniDB files under another anime has no AniDB ID of its own, so it finds its list by its AniList ID. The first is which TVDB season 0 episode a film filed under a series is. That lets the indexer offer such a film to Sonarr under a title it can match. The second is which TVDB seasons an absolute-numbered run's episodes fall in. That lets a pack with no season in its file names carry its season, and lets the report judge a split show against its own seasons.

### Overrides

To pin an entry the ID map misses or gets wrong, put a `/config/overrides.json` beside the config. It is a JSON array of records keyed by `anilist_id`, applied ahead of animap, and the file is optional. Each record takes these fields:

| Field | Meaning |
| --- | --- |
| `anilist_id` | Required. The SeaDex entry the record maps. |
| `type` | `movie` routes to Radarr. Anything else routes to Sonarr. |
| `tvdb_id` | The TVDB series ID. |
| `tmdb_movies` | An array of TMDB movie IDs, as numbers. |
| `imdb_ids` | An array of IMDb IDs, as strings. |
| `anidb_id` | The AniDB ID whose mapping list the entry uses. The episode and season facts still come from animap. |
| `season_tvdb` | The TVDB season to compare against. |
| `season_kind` | Whether upstream maps a TVDB season for the entry at all, `present` or `absent`. |

With `season_kind: present`, a positive `season_tvdb` compares against that season. A `season_tvdb` of 0 means the entry lands in Sonarr's season 0, where the indexer offers it but nothing compares it. With `season_kind: absent`, the entry is judged against the whole series. Leave `season_kind` out and a positive `season_tvdb` still picks that season. Only an entry without one is routed by its `type`.

These are the only field names read. Other spellings, such as `imdb_id`, `themoviedb_id` and `season`, are ignored with a warning naming the key. An override replaces the whole mapping record for its `anilist_id`, with no field-by-field merge. When you correct an entry animap already has, restate every field the entry needs.

### Title fallback

When an entry is not in the ID map, seadex-scout fetches its titles and format from AniList and tries a careful title-plus-year match against your library. The match must be exact and have a single candidate. An ambiguous match is skipped rather than guessed.

## Release classification

Each SeaDex release and each library file is sorted into one vocabulary. That covers the release group, the tracker, the resolution, the codec, dual audio, and the kind, which is `remux`, `encode` or `unknown`. Nyaa is a public tracker and AnimeBytes a private one. A release that cannot be classified is `unknown` and is never silently dropped. The comparison works by group. An item is aligned when a recommended release group is already on it.

### Revisions

The comparison also reads the release revision. That is a `v2` or `v3` token, such as `04v2`, `S01E05v3` or `[v2]`, a `PROPER`, or a `REPACK` or `RERIP`. `REPACK2` counts one higher than `REPACK`.

On your side, the revision is the one Sonarr or Radarr recorded when it imported the file, which survives a rename. Only when the app reports none does a token in the file's scene name or path count. On the SeaDex side, the revision is read from the release's file names, never from the entry notes, with the same rules Sonarr and Radarr use. A file from the very torrent SeaDex lists can therefore never look older than it.

Both sides compare their newest revision. Your side is the newest revision your files of a group carry, across the season, the movie, or all the seasons an entry spans. SeaDex's side is the newest file SeaDex lists across that group's best releases. A group that reissues one episode of a pack as `v2` therefore lists `v2`.

When your newest revision is older than SeaDex's for every SeaDex best group you hold, the item is not aligned. The daemon logs a `newer_revision` finding and the report says `have_older_revision`. That includes a library that holds the pack without the reissued episode. Holding the listed revision or a newer one stays aligned. So does missing revision evidence on either side, such as a SeaDex release with no version or a library file with no recorded revision. A missing token is never read as an older release.

## Filters

`filters.exclude_remux` and `filters.require_dual_audio` shape the log findings only. The report and the indexer apply neither. In the indexer, Sonarr and Radarr filter through your quality profile and Custom Formats. All filters are optional:

- `filters.exclude_remux`, `false` by default, stops releases classified as `remux` from counting as a recommendation. The default keeps them, because on SeaDex a remux is often the best release.
- `filters.require_dual_audio`, `false` by default, drops releases that are not dual audio.
- `filters.exclude_specials`, `false` by default, drops OVA, ONA and special entries from the findings and the report.
- `animebytes`, `false` by default, is the one tracker switch. The public trackers SeaDex lists, Nyaa, AnimeTosho and RuTracker, always count. The private tracker AnimeBytes counts only when you turn this on. Then a finding carries every source, so a release on both Nyaa and AnimeBytes shows both links. An AnimeBytes link is the torrent page you open as a member, so seadex-scout needs no tracker credentials for it.
- `arr_tags.include` and `arr_tags.exclude` check only items carrying an include tag, and never items carrying an exclude tag. An exclude wins when an item has both.

## The indexer

When a Prowlarr Torznab URL is configured, the daemon serves a [Torznab](https://torznab.github.io/spec-1.3-draft/) feed of SeaDex releases for Sonarr and Radarr, in the same process as the checks. Add it to Sonarr and Radarr, directly or through Prowlarr, and they parse, match and grab through their own engines, profiles and history, as for any other indexer. [Torznab feed setup](torznab-indexer.md) walks through it.

### Searches and RSS

The feed handles its two kinds of request in two ways. A search, the automatic or interactive kind that carries a query, is passed to Prowlarr's Nyaa and AnimeBytes Torznab endpoints and filtered to what SeaDex lists. Its download links are Prowlarr's own, so no tracker passkey is needed for it.

The periodic RSS check carries no query. For it, the feed builds the SeaDex list itself. It titles each item from SeaDex's own file names, with a public Nyaa `.torrent` link or an AnimeBytes link built from your `ab_passkey`.

If every upstream query fails, a search answers with a Torznab error rather than an empty result. Sonarr and Radarr then record a failed search instead of concluding there were no results.

### Season searches only

The feed answers whole-season searches, not single-episode ones. SeaDex tracks season packs, so the feed answers a season search with the pack. For a single-episode query it returns nothing, without contacting a tracker. Specials and movies are single releases and are always answered. That is why Sonarr's **Anime Standard Format Search** option must be ticked on the indexer. Without it, Sonarr sends only single-episode queries.

### Markers

Every item, from a search or from RSS, carries a marker. That is a download volume factor for the tier plus a `scene` tag. SeaDex's best release gets the factor `0.75`, which with the `scene` tag Sonarr and Radarr record as the Indexer Flags Freeleech25 and Scene. An alternative gets `0.25`, recorded as Freeleech75 and Scene. Map that pair to a Custom Format with both flags required, and Sonarr and Radarr prefer SeaDex's pick.

Requiring both flags matters because some trackers use real 25% and 75% freeleech, OldToonsWorld among them, and their indexer definitions cannot set Scene. A Custom Format on the tier flag alone keeps matching the feed.

### Categories and titles

Each item's category follows the entry's real media type and the app its library item resolved to. A series, OVA or special is `5070`, Anime, for Sonarr. A film is `2000`, Movies, for Radarr. A film whose library item is a Sonarr series is also offered under Anime, because Sonarr subscribes only to Anime.

A film that TVDB files as a special of a series you have in Sonarr is also served as a second item titled `<Series> S00Exx` under Anime, so Sonarr's parser can match it. A season pack whose file names carry no season gets the season of the one TVDB season the mapping places it in.

When the newest file of a release carries a newer revision, a `v2`, `PROPER` or `REPACK`, and the title would otherwise read an older one, the title gains that revision. That holds even when only one reissued episode of a pack carries it. Two examples are `Show S01 [v2] 1080p [Grp]` and `86 Eighty Six S01 REPACK 1080p [koala]`. Sonarr and Radarr then see the upgrade the daemon reports. The token goes before the release flags, never at the end, where Sonarr would read it as the release group. A search result keeps the tracker's own title.

# Fixing a wrong or missing match

This page shows what to do when a SeaDex entry does not match a show you have in Sonarr or Radarr, or matches the wrong show, season or film. You can correct it yourself with an overrides file, then report it so the fix reaches everyone.

## Spotting a wrong or missing match

seadex-scout links each SeaDex entry to your library through the [animap](https://github.com/cplieger/animap) ID map, which turns the entry's AniList ID into a TVDB, TMDB or IMDb ID. Some entries are missing from the map, or their record has no ID for your Sonarr or Radarr server. For those, seadex-scout matches the entry's titles against your library instead, ignoring case, spaces and punctuation. It also checks the year when AniList and your library both have one. It skips the entry when more than one show fits.

A wrong or missing ID shows up in the [report](how-it-works.md#the-report) and on the dashboard in these ways:

- The report lists a show you have under `not_on_seadex`, or does not list it at all, though the show has a page on [SeaDex](https://releases.moe).
- A row sits under `no_file (season not found)` though you have that season. The dashboard counts it as Season not found. The entry points at another season, or Sonarr files that season elsewhere. A film you keep in Radarr also sits here when the map names its special episode but has no TMDB or IMDb ID for it. The report then looks for it only in the series' Sonarr specials, and its notes name the missing episode, such as `missing S00E09`. Add the film's TMDB movie ID in `tmdb_movies` with an overrides.json record, as [A film or special filed under a series](#a-film-or-special-filed-under-a-series) shows.
- A row's `seadex` link opens a different show than the row's title. The entry points at the wrong TVDB, TMDB or IMDb ID.
- A film or special sits under `unattributed (episode not known)`. The dashboard counts it as Episode not known. The map files it under its series' Sonarr specials but does not say which special episode it is, so it is not compared. A map record that names its episodes fixes it, as [Getting it fixed for everyone](#getting-it-fixed-for-everyone) explains. A film you keep in Radarr also sits here when the map neither names its episode nor has a TMDB or IMDb ID for it. Adding its TMDB movie ID in `tmdb_movies` fixes that too.
- The log says `anilist record unusable for matching; add an overrides.json entry to map it directly`, with the entry's `al_id`.

## Finding the IDs

- The AniList ID is the number at the end of the SeaDex page address, such as `12345` in `https://releases.moe/12345`. Log lines and the JSON report call it `al_id`.
- The TVDB ID is on the show's page at [thetvdb.com](https://thetvdb.com). Use the season number Sonarr shows, which is TVDB's.
- The TMDB movie ID is the number in the film's address at [themoviedb.org](https://www.themoviedb.org), such as `54321` in `themoviedb.org/movie/54321`.
- The AniDB ID is the number in the address at [anidb.net](https://anidb.net), such as `11111` in `anidb.net/anime/11111`.

To see what animap has for an entry, download its file and look the entry up with `jq`:

```sh
curl -fLO https://github.com/cplieger/animap/releases/latest/download/animap.json
jq '.records[] | select(.anilist_id == 12345)' animap.json
```

Copy only the fields the examples below use. The overrides file reads no other field, so pasting animap's whole record logs an unknown-keys warning. In the overrides file, animap's `tvdb_season` is `season_tvdb` and its `tmdb_movie_ids` is `tmdb_movies`. When animap's record has `tvdb_season`, write `"season_kind": "present"`, because animap has no `season_kind` field.

## Fixing it now with overrides.json

Create a file named `overrides.json` in your `config` folder, beside `config.yaml`. seadex-scout reads it as `/config/overrides.json`, and it wins over animap for every entry it names. The file is one JSON array with one record per entry, keyed by `anilist_id`. Each example below is a complete file, so put several records in one array when you fix several entries.

A record replaces animap's whole record for that entry, with no field-by-field merge. Write every field the entry needs, even the ones animap already has right. Keep animap's `anidb_id` when it names the right anime, because the indexer uses it to label season packs. [How seadex-scout works](how-it-works.md#overrides) lists every field.

### A series with the wrong TVDB ID

```json
[
  {
    "anilist_id": 12345,
    "type": "TV",
    "tvdb_id": 67890,
    "season_kind": "present",
    "season_tvdb": 1
  }
]
```

Put in the entry's AniList ID, the right TVDB ID and the season the entry covers.

### A season mapped to the wrong season

```json
[
  {
    "anilist_id": 12345,
    "type": "TV",
    "tvdb_id": 67890,
    "season_kind": "present",
    "season_tvdb": 2
  }
]
```

Keep the TVDB ID animap has and change `season_tvdb`. For an entry that covers a whole series, write `"season_kind": "absent"` and leave out `season_tvdb`. seadex-scout then compares it with every season you have.

### A film or special filed under a series

```json
[
  {
    "anilist_id": 12345,
    "type": "MOVIE",
    "tmdb_movies": [54321],
    "imdb_ids": ["tt1234567"],
    "tvdb_id": 67890,
    "season_kind": "present",
    "season_tvdb": 0,
    "anidb_id": 11111
  }
]
```

A `MOVIE` record is looked up in Radarr by `tmdb_movies`, then by `imdb_ids`. When neither is in Radarr, it falls back to the series `tvdb_id` in Sonarr. `season_tvdb` 0 then places it in Sonarr's specials. Copy `anidb_id` from animap's record when it has one. animap's record for that anime says which special episodes the film is, which lets the report compare it and the indexer name it to Sonarr, whichever app holds the film. Both use it only while `tvdb_id` matches animap's record, because the episode numbers belong to that series. Without it the report lists the film as `unattributed`.

For an OVA or a special you keep in Sonarr's specials, write `"type": "OVA"` or `"type": "SPECIAL"`, keep `tvdb_id`, `season_kind` and `season_tvdb`, and leave out the movie IDs.

### An entry with no mapping at all

```json
[
  {
    "anilist_id": 12345,
    "type": "MOVIE",
    "tmdb_movies": [54321]
  }
]
```

This links a film animap does not know to your Radarr film. For a series animap does not know, use the record from the first example.

### Checking that it worked

Run a report:

```sh
docker exec seadex-scout /seadex-scout report
```

The report reads `overrides.json` itself and prints its log lines in your terminal. `mapping: applied overrides` with a `count` means the file was read. Then open the new report in `config/reports` and find the entry's row. These lines mean a record was not used as you meant:

- `mapping: overrides.json malformed, pinned mappings not applied` means the file is not one valid JSON array, or a field holds the wrong kind of value. An ID in quotes and `imdb_ids` written without `[ ]` are the common cases. No record in the file is used until you fix it.
- `mapping: overrides.json unreadable` means the container user cannot read the file. For permissions, run `sudo chown 1000:1000 config/overrides.json`, or use your `PUID` and `PGID`. The same line appears when `overrides.json` is a folder, a link that leads outside the `config` folder, or larger than 4 MiB.
- `mapping: overrides contain unknown keys, ignored` means a field name is misspelled or is not one the file reads, such as `imdb_id`, `themoviedb_id`, `season` or animap's `mapping_list`.
- `mapping: overrides carry no arr identifier and un-map their entry` means a record has no ID for its app, so its entry now matches nothing. A `MOVIE` record needs `tmdb_movies` or `imdb_ids`, and every other type needs `tvdb_id`.
- `mapping: overrides with missing or invalid anilist_id skipped` means a record has no positive `anilist_id`.

The running container reads the file again at each start and at every full pass. With a `poll_interval` of 24 hours or less, the default included, a full pass runs at least once a day. A longer `poll_interval` makes every check a full pass. To apply a change now, run `docker restart seadex-scout`. With `poll_interval: off`, each `poll` run reads the file itself, so no restart is needed.

## Getting it fixed for everyone

To build `animap.json`, animap joins two datasets. [anime-offline-database](https://github.com/cedya77/anime-offline-database) links each AniList entry to its AniDB anime. [Anime-Lists](https://github.com/Anime-Lists/anime-lists) places each AniDB anime on TVDB, TMDB and IMDb. On top of those, animap adds AniDB's episode counts, read through a mirror, and its own corrections. Each of the three projects takes reports for its own part.

Look the entry up in `animap.json` as shown above, open its `anidb_id` at anidb.net, and follow the first case that fits:

- If the record has `anidb_parent` instead of `anidb_id`, animap placed that special under its parent anime itself. Report it on [animap](https://github.com/cplieger/animap/issues).
- If `anidb_id` is the right anime, the anime is placed wrongly. That covers a wrong TVDB, TMDB or IMDb ID, season or episode offset, and a film or special in the wrong place. Look the anime up in Anime-Lists' file:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/Anime-Lists/anime-lists/master/anime-list-master.xml | grep -A3 'anidbid="11111"'
  ```

  If the value is wrong there, fix it in Anime-Lists with a pull request that edits `anime-list-master.xml`, or open an issue there. Link the anime's AniDB page and its TVDB season page in Aired Order. Give the air dates that line the episodes up. If Anime-Lists has it right, the difference comes from animap, so report it on animap.
- If the lookup prints nothing, or `anidb_id` is missing or names another anime, look the entry up on AniDB. If AniDB lists it only as specials of another anime, animap can file it under that anime, so report it on animap. If AniDB has it as an anime of its own, anime-offline-database groups it wrongly. Report it on [anime-offline-database](https://github.com/cedya77/anime-offline-database/issues). Use its merge request form for entries that should be one anime, and its split request form for one entry that holds two. Both forms ask for the `sources` addresses of the entries involved.
- If the record has `tvdb_season` 0 and no `tvdb_placement`, the map does not say which TVDB special episode each of the anime's episodes is. animap works that out from a mapping list row or an `episodeoffset` in the anime's Anime-Lists entry. It publishes the answer only when every episode has one. Until then, the report does not compare it, and the indexer names it to Sonarr only when it is a film and a mapping list row gives the one special episode it is. Add the missing row or offset to Anime-Lists with a pull request. Or report it on animap, which can add it while the pull request waits. Link the anime's AniDB page and the TVDB specials it is.
- If you cannot tell which project is wrong, open an issue on animap. Give the AniList or AniDB ID, what the file says and should say, and the AniDB, TVDB or TMDB page that shows it.

Every 3 hours, animap rebuilds its file from the newest Anime-Lists commit, so an Anime-Lists fix reaches animap within hours. An anime-offline-database fix waits for that project's next weekly release and animap's update to it.

At every full pass, seadex-scout checks for a new `animap.json` and downloads it only when it changed. A fix therefore reaches you at your first full pass after animap's release, with no seadex-scout update. On the default schedule, that is within a day. Once the `jq` lookup above shows the fix in `animap.json`, remove the record from `overrides.json`, because a record keeps winning over the map.

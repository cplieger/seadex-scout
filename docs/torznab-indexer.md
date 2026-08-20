# Torznab feed setup

How to enable seadex-scout's optional
[Torznab feed](../README.md#indexer-torznab-feed) and wire it into Prowlarr and
Sonarr/Radarr. There is no separate command or container: the daemon starts the
feed the moment you give it a Prowlarr Torznab URL. Leave both URLs empty and the
daemon binds no HTTP port at all, staying socket-less for an alert-only
deployment.

## 1. Point the feed at Prowlarr

In Prowlarr, add **Nyaa** and **AnimeBytes** as indexers if you have not. Each
indexer's page shows its **Torznab Url** (like `http://prowlarr:9696/1/api`);
copy both, and get a Prowlarr API key from Prowlarr → Settings → General. Fill
in the `indexer` section and restart:

```yaml
indexer:
  feed_api_key: "a-random-string"                   # generate with: openssl rand -hex 16
  nyaa_torznab_url: "http://prowlarr:9696/1/api"    # "" disables Nyaa
  ab_torznab_url: "http://prowlarr:9696/2/api"      # "" disables AnimeBytes
  prowlarr_api_key: "${SEADEX_SCOUT_PROWLARR_KEY}"  # secret, never logged
  ab_passkey: "${SEADEX_SCOUT_AB_PASSKEY}"          # AnimeBytes passkey; required for the AB RSS feed, "" leaves it off
```

The port is fixed at `:9118` (published by your compose port mapping, not a
config key).

One Prowlarr setting is worth changing while you are there: on the **Nyaa**
indexer, set **Sort requested from site** (under the indexer's advanced settings)
to `seeders` instead of the default created/date. Nyaa returns a single page
of results and Prowlarr never requests the pages behind it, so
date-sorted results bury older well-seeded BD and batch releases beyond that
first page. Sorting by seeders surfaces exactly those, which materially improves
how many SeaDex picks a search can return for older shows.

For **searches**, the download links are Prowlarr's own proxy URLs, so no passkey
is needed; Prowlarr grabs with the credentials it holds. The **AnimeBytes RSS
feed** is the one exception: SeaDex never publishes AnimeBytes download links, so
the feed builds them from your `ab_passkey` (the token in your AnimeBytes
RSS/announce URL). Leave it empty and the `/ab` feed has nothing grabbable to
serve, so it returns a clear error and Prowlarr's save-test fails until you set
it. Nyaa is public and needs nothing. The passkey rides in the AnimeBytes feed's
links, so keep the endpoint on your LAN (see
[Security](../README.md#security)).

## 2. Add the feed to Sonarr/Radarr

Settings → Indexers → Add → **Torznab** (Custom):

- **URL:** the feed is **per-tracker**. Add Nyaa as
  `http://seadex-scout:9118/nyaa` and AnimeBytes as
  `http://seadex-scout:9118/ab`, as two Torznab indexers. There is no combined
  endpoint; serving each tracker on its own path is what lets you gate their
  search types independently (see [Per-tracker search gating](#per-tracker-search-gating)).
- **API Key:** the `indexer.feed_api_key` from step 1.
- **Categories:** `5070` (Anime) in Sonarr, `2000` (Movies) in Radarr.
- **☑ Anime Standard Format Search, required.** This is what makes Sonarr issue
  the whole-season query the feed answers. Without it Sonarr sends only
  per-episode queries, which the feed ignores, and you get nothing.

You can instead add it to Prowlarr and let Prowlarr sync it to the arrs. Either
works, and there is no query loop, but the Anime Standard Format Search toggle
must still end up set on the indexer as the arr sees it.

## 3. Create two Custom Formats

Settings → Custom Formats → Add. Each gets a single condition of type **Indexer
Flag**:

| Custom Format | Condition (Indexer Flag) | Matches | From `downloadvolumefactor` |
| --- | --- | --- | --- |
| `SeaDex (best)` | `Freeleech25` | SeaDex's best pick | `0.75` |
| `SeaDex (alt)` | `Freeleech75` | a SeaDex-listed alt | `0.25` |

Leave the condition's "negate" and "required" boxes unchecked. The flag names are
exactly `Freeleech25` and `Freeleech75`; Sonarr/Radarr derive them from the feed's
`downloadvolumefactor`.

## 4. Score them on your anime quality profile, and only that profile

Settings → Profiles → your anime profile → Custom Formats: give `SeaDex (best)` a
high positive score (for example `100`) and `SeaDex (alt)` a lower one (for
example `50`). Sonarr/Radarr now prefer, and upgrade to, SeaDex's pick over an
equivalent non-SeaDex release. **Scope the scores to the anime profile:** it keeps
the markers from colliding with genuine AnimeBytes Freeleech25/75 releases in your
non-anime libraries.

## Per-tracker search gating

To use a tracker for some search types only (for example Nyaa, which is public, on
manual searches, and AnimeBytes on everything), set the arr's per-indexer flags.
The arr already enforces the per-indexer **Enable RSS / Enable Automatic Search /
Enable Interactive Search** flags, and it is the only component that can: a Torznab
request never carries the search _type_, and only RSS (the no-query "recent" feed)
is distinguishable. So
the feed exposes each tracker on its own path and lets the arr decide when to hit
each:

| Feed | Path | Or subdomain |
| --- | --- | --- |
| Nyaa | `…/nyaa` | `nyaa.example.com` |
| AnimeBytes | `…/ab` | `ab.example.com` |

Add the per-tracker feeds as **two** Torznab indexers (each still needs Anime
Standard Format Search on), then set their flags in Sonarr/Radarr (Settings →
Indexers). To make Nyaa manual-only, untick **Enable RSS** and **Enable Automatic
Search** on the Nyaa indexer and leave the AnimeBytes one fully enabled. Adding
them through Prowlarr with a sync profile works too; the flags must end up on the
indexer as the arr sees it.

If seadex-scout runs apart from the arrs behind a reverse proxy, the subdomain
form is cleaner than the path: point `nyaa.example.com` and `ab.example.com` at
the one `:9118` and the feed picks the tracker from the hostname, with no path
rewrite and no second port. The proxy must pass the `Host` header through
unchanged (the default for Caddy and nginx `reverse_proxy`).

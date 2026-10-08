# Torznab feed setup

This page shows how to turn on seadex-scout's optional indexer, a [Torznab feed](how-it-works.md#the-indexer), and connect it to Prowlarr, Sonarr and Radarr. There is no separate command or container. The daemon starts the feed as soon as you give it a Prowlarr Torznab URL. Leave both URLs empty and the daemon opens no network port at all.

## 1. Point the feed at Prowlarr

In Prowlarr, add **Nyaa** and **AnimeBytes** as indexers if you have not. Each indexer's page shows its **Torznab Url**, such as `http://192.0.2.10:9696/1/api`. Its host must be an address seadex-scout can reach, such as Prowlarr's LAN address, not `localhost`. Copy both, and copy Prowlarr's API key from Prowlarr, Settings, General. Fill in the `indexer` section of `config.yaml` and restart:

```yaml
indexer:
  feed_api_key: "a-random-string"                     # generate with: openssl rand -hex 16
  nyaa_torznab_url: "http://192.0.2.10:9696/1/api"  # "" disables Nyaa
  ab_torznab_url: "http://192.0.2.10:9696/2/api"    # "" disables AnimeBytes
  prowlarr_api_key: "${SEADEX_SCOUT_PROWLARR_KEY}"    # secret, never logged
  ab_passkey: "${SEADEX_SCOUT_AB_PASSKEY}"            # AnimeBytes passkey; required for the AB RSS feed, "" leaves it off
```

The port is fixed at `9118`. It is not a config key. Add it to the seadex-scout service in `compose.yaml` so Sonarr and Radarr can reach it:

```yaml
    ports:
      - "9118:9118"
```

While you are in Prowlarr, change one setting on the **Nyaa** indexer. Under its advanced settings, set **Sort requested from site** to `seeders` instead of the default created date. Nyaa returns a single page of results and Prowlarr never asks for the pages behind it. Date-sorted results therefore bury older, well-seeded BD and batch releases beyond that first page. Sorting by seeders brings those up, so a search returns more SeaDex picks for older shows.

For searches, the download links are Prowlarr's own proxy links, so no passkey is needed. Prowlarr grabs with the credentials it holds. A search for a special episode also returns SeaDex's own releases for it, which link straight to Nyaa, or to AnimeBytes through your `ab_passkey`. Without the passkey, an `/ab` special search returns only what Prowlarr finds. The AnimeBytes RSS feed is the one exception. SeaDex never publishes AnimeBytes download links, so the feed builds them from your `ab_passkey`, the token in your AnimeBytes RSS or announce URL. Leave it empty and the `/ab` feed has nothing to offer, so it returns a clear error and Prowlarr's save test fails until you set it. Nyaa is public and needs nothing.

The passkey rides in the AnimeBytes feed's links, so keep the feed on your local network, as [Security](../README.md#security) explains.

The RSS check lists only releases SeaDex picks after seadex-scout first starts. Older ones reach Sonarr and Radarr through search. While a tracker's RSS check has nothing in the requested category, it returns one placeholder item titled `seadex-scout online - no curated releases yet`, so the save test in Prowlarr, Sonarr and Radarr passes on a fresh install. RSS sync skips it. Do not grab it from Prowlarr's search page, because the download fails and counts as an indexer failure. When real releases first replace it, Sonarr logs one warning that the RSS sync did not cover the period since 1970. You can ignore it.

## 2. Add the feed to Sonarr and Radarr

In Sonarr or Radarr, open Settings, Indexers, Add, and pick **Torznab** under Custom. Fill in these fields:

- The feed is per tracker. Set **URL** to the address Sonarr reaches seadex-scout at, with `/nyaa` for Nyaa and `/ab` for AnimeBytes, such as `http://192.0.2.10:9118/nyaa` and `http://192.0.2.10:9118/ab`, as two Torznab indexers. If Sonarr shares a Docker network with seadex-scout, `http://seadex-scout:9118/nyaa` works too. There is no combined endpoint. A path per tracker lets you choose each tracker's search types on its own, as [Per-tracker search gating](#per-tracker-search-gating) shows.
- Set **API Key** to the `indexer.feed_api_key` from step 1.
- Set **Categories** to `5070` (Anime) in Sonarr and `2000` (Movies) in Radarr. The feed tells Sonarr and Radarr apart by them, so keep each app to its own. A film reaches Sonarr under its series title and special episode, and Radarr under its own title. A film that cannot be named that way reaches Radarr only.
- Tick **Anime Standard Format Search**. This is required. It makes Sonarr send the whole-season search the feed answers. Without it, Sonarr sends only single-episode searches, which the feed answers only for specials, and you get nothing else.

You can add the feed to Prowlarr instead and let Prowlarr sync it to Sonarr and Radarr. Either works, and there is no search loop. The Anime Standard Format Search option must still end up ticked on the indexer as Sonarr sees it.

## 3. Create two Custom Formats

Every item the feed marks carries two signals. Its `downloadvolumefactor` names the tier, and a `scene` tag sits beside it. Sonarr and Radarr record both as Indexer Flags:

| Marked as | `downloadvolumefactor` | Sonarr flags | Radarr flags |
| --- | --- | --- | --- |
| SeaDex best | `0.75` | `Freeleech25`, `Scene` | `G Freeleech25`, `G Scene` |
| SeaDex alt | `0.25` | `Freeleech75`, `Scene` | `G Freeleech75`, `G Scene` |

Open Settings, Custom Formats, Add. Give each Custom Format two conditions of type **Indexer Flag**, and tick **Required** on both:

| Custom Format | Sonarr conditions | Radarr conditions |
| --- | --- | --- |
| `SeaDex (best)` | `Freeleech25` + `Scene` | `G Freeleech25` + `G Scene` |
| `SeaDex (alt)` | `Freeleech75` + `Scene` | `G Freeleech75` + `G Scene` |

Leave **Negate** unticked. Require both flags because some trackers use real 25% and 75% freeleech, OldToonsWorld among them, and their own releases then carry `Freeleech25` or `Freeleech75` too. The Cardigann definitions most Prowlarr and Jackett trackers use cannot set the Scene flag at all. Of Prowlarr's built-in indexers, only GreatPosterWall, a movie tracker, can set Scene beside a 25% or 75% factor.

A Custom Format with only the tier flag, `Freeleech25` or `Freeleech75` alone, still matches every item the feed marks. It also matches those trackers' freeleech releases.

## 4. Score them on your anime quality profile

Open Settings, Profiles, your anime profile, Custom Formats. Give `SeaDex (best)` a high positive score, such as `100`, and `SeaDex (alt)` a lower one, such as `50`. Sonarr and Radarr now prefer, and upgrade to, SeaDex's pick over an equal non-SeaDex release.

An RSS item whose newest file carries a newer revision of the release, a `v2`, `PROPER` or `REPACK`, has that revision in its title. That holds even when only one reissued episode of a pack carries it. Sonarr or Radarr can then upgrade a v1 it already holds from the same group. Whether it does depends on the **Propers and Repacks** setting under Settings, Media Management. `Prefer and Upgrade` upgrades automatically. The other two choices leave the upgrade to you.

## Per-tracker search gating

You can use a tracker for some search types only, for example public Nyaa for manual searches and AnimeBytes for everything. Do it with the per-indexer flags in Sonarr or Radarr. They already enforce **Enable RSS**, **Enable Automatic Search** and **Enable Interactive Search** per indexer, and they are the only component that can. A Torznab request never says which type of search sent it, and only RSS, the no-query recent-releases check, can be told apart. So the feed puts each tracker on its own path and lets Sonarr and Radarr decide when to use each one:

| Feed | Path | Or subdomain |
| --- | --- | --- |
| Nyaa | `…/nyaa` | `nyaa.example.com` |
| AnimeBytes | `…/ab` | `ab.example.com` |

Add the two feeds as two Torznab indexers, each with Anime Standard Format Search ticked, then set their flags under Settings, Indexers. To make Nyaa manual-only, untick **Enable RSS** and **Enable Automatic Search** on the Nyaa indexer and leave the AnimeBytes one fully enabled. Adding them through Prowlarr with a sync profile works too. The flags must end up on the indexer as Sonarr and Radarr see it.

If seadex-scout runs apart from Sonarr and Radarr behind a reverse proxy, the subdomain form is simpler than the path. Point `nyaa.example.com` and `ab.example.com` at the one port `9118`, and the feed picks the tracker from the hostname, with no path rewrite and no second port. The proxy must pass the `Host` header through unchanged. Caddy's `reverse_proxy` does this by default. In nginx, add `proxy_set_header Host $host;`, because its default sends the upstream's own name.

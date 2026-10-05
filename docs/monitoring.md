# Monitoring and alerts

This page describes what seadex-scout logs, how its health check works, and the Loki alert rules it ships. Read it when you want alerts on findings or on a stalled app.

## Log lines

seadex-scout has no metrics endpoint. Its only output is its log, written as JSON to standard output. It opens no network port unless you configure the [indexer](torznab-indexer.md), which listens on the fixed port `9118`.

A finding is one line at `warn` with `msg="better release available"`. It carries the title, the AniList ID, the current and recommended groups, and the release's classification. It also carries one link per source you can get it from, as `nyaa_url`, `public_url` with `public_tracker`, or `ab_url` with `ab_tracker`, so an alert can show a clickable link straight from the labels. [`alerts/logql.yaml`](../alerts/logql.yaml) names the attributes it groups by. A `better_release` finding also carries `current_tier`. It is `alt` when SeaDex lists the group you have without recommending it, and `unlisted` when SeaDex does not list it at all.

Every finding line also carries `alt_groups`, the groups SeaDex lists only as alts for that entry, lower-cased and joined with commas. AnimeBytes alts are left out when `animebytes` is off. It is empty when the entry has no alt.

A newer revision of a group you already hold logs the same `warn` message with `status=newer_revision`, `current_revision` and `recommended_revision`, such as `v1` and `v2`. Its `seadex_tags` value starts with `newer-revision`, so existing better-release alert rules fire on it too.

Informational cases log at `info`. Their statuses are `incomplete`, `theoretical_best`, `mixed_group_manual` and `unverifiable`.

Every pass ends with a completion line. A healthy pass logs `tick complete` or `cycle complete`. A pass that ran but could not compare logs `tick degraded` or `cycle degraded` at `warn`, with a `reason`, when an upstream outage or a safety check skipped the comparison. Every full pass also logs `reconcile complete`. Report mode logs one `report item` line per anime.

Every full pass whose library read was complete also logs one `library summary` line. It carries the report's counts per verdict, `anime_items`, `items_with_entry`, `items_all_best` and `items_all_best_or_alt`. `items_all_best_or_alt` counts the anime where every season or film you have is SeaDex's best or an alt, so it includes `items_all_best`. Report mode logs the same item counts on its `report summary` line. The two messages differ, so a count never adds a report run to a daily pass. The `attrs:` lines in [`alerts/logql.yaml`](../alerts/logql.yaml) list the stable attributes of each message.

A finding is logged again on every pass for as long as it is true, so an alert keeps firing until you upgrade the release. It stops when a later pass no longer finds it.

A problem that lasts, such as SeaDex being unreachable, logs an `error` line with a `condition` attribute on every pass until it clears. It saves these problems in its state, so a restart does not clear an alert that is still true.

## Health

The image's Docker `HEALTHCHECK` runs the `seadex-scout health` subcommand. It reads a marker file at `/tmp/.healthy`, so it needs no shell and no port. Each completed pass refreshes the marker, and the marker reflects whether the last read of your Sonarr or Radarr library worked.

A failed library read marks the container unhealthy, because a restart or a config fix can recover it. A SeaDex, mapping or AniList outage keeps the container healthy, because a restart cannot fix an outage somewhere else, and the existing findings stay in place.

The container also turns unhealthy when no pass finishes within three poll intervals, and never sooner than 3 hours. The log then says `no cycle has completed within the health lease; marking unhealthy`. With `poll_interval: off` this check is off, because an outside scheduler runs the passes.

## Dashboard

Every release attaches `grafana-dashboard.json` to its GitHub Release, with a `.sha256` file and a signature. The same file is published as `ghcr.io/cplieger/seadex-scout/dashboard:<version>`. Import it into Grafana with **Dashboards**, then **New**, then **Import**. It reads Loki only.

The dashboard has three variables at the top. **Loki** picks your Loki data source. **Container** is the value of the `container` label your log collector puts on seadex-scout's lines, `seadex-scout` by default.

**Optional upgrades** decides whether the upgrades list shows upgrades where you already have a SeaDex alt. **Hide** is the default and leaves them out. Pick **Show** to see them too. The Upgrades count at the top follows the same setting, so the count matches the list.

The dashboard has three rows:

- At a glance shows the status, the number of upgrades and of findings to check by hand, and two shares of your anime. Library overview on its right counts SeaDex entries by what you have, one per season or film SeaDex lists.
- Upgrades lists every upgrade, newest first, with the time it was first seen. It also lists the findings seadex-scout could not decide on its own, under Check by hand. Below them, a stacked graph counts your anime at each daily check. Its four parts are at SeaDex best, at SeaDex alt, not at best or alt, and not on SeaDex, so the total is every anime in your library. Not at best or alt covers an anime with any season on another group, an older version, no files found or an untagged release.
- Scout health is collapsed. It shows why the status is not `✓`, when the last checks finished, how often Sonarr and Radarr read the Torznab feed, and how many releases the feed newly offered.

The SeaDex best tile is the share of your anime where everything you have is SeaDex's best release. The SeaDex alt tile is the share where you have best or alt releases with at least one alt. SeaDex's best is often a remux, so an anime you keep as encodes usually counts as alt.

Three Library overview bars need a word. Season not found means no files sit where the entry maps, either because they are missing or because Sonarr files that season elsewhere, such as under TVDB's specials. Unmapped specials are films or specials in Sonarr's specials that seadex-scout cannot tie to one entry, so it offers them in the feed and does not compare them. Untagged release means your file or SeaDex's release names no group, so seadex-scout cannot compare them. When neither names a group, the two count as the same release.

In the upgrades list, You have, SeaDex best and SeaDex alt name the release groups. On a newer version they also show the version, such as `smol v1` against `smol v2`. SeaDex alt shows `-` when SeaDex lists no alt.

Each title opens the series or film in Sonarr or Radarr. The Nyaa, AnimeBytes and Other columns open the recommended release on that site. A column shows only when some upgrade has a link there, so AnimeBytes shows only when you turn AnimeBytes on.

The Why column says what kind of upgrade it is. `v2 available` is a fixed version of the group you have. `SeaDex alt` means you have an alt and SeaDex's best is the upgrade. `Neither best nor alt` means SeaDex lists your group as neither its best nor an alt.

The status shows `✓`, `!` or `✗`, built from the same conditions as the shipped alert rules. A tick means checks run and nothing fails. An exclamation mark means SeaDex, AniList or the anime ID map is failing, and nothing needs fixing on your side. A cross means an error you can fix or checks that stopped. Scout health says which.

First seen is the earliest time a finding was logged within the selected time range. A finding older than the range, or older than the logs your Loki keeps, shows the earliest time still held. Widen the range to look further back. Loki's `max_query_length` setting can refuse a very long range. If it does, the upgrades still show with First seen empty. First seen needs a Loki version whose `label_format` supports `__timestamp__`. Loki 3.7 is known to work.

The two share tiles, Library overview and the graph show the SeaDex view, like the report. Your remux and dual-audio filters are not applied, so their numbers can disagree with Upgrades available. An anime whose only SeaDex entries have no file, or are only offered in the feed, has an entry but is never at best or alt.

These numbers come from the `library summary` line each full daily check logs, so they lag by up to a day. A full check whose library read was incomplete logs no summary. They then keep the previous summary until it is 26 hours old and are empty after that, until a complete check logs a new one.

With `poll_interval: off`, each check runs in a `docker exec` child whose lines never reach the container log. The status then shows `✗` and the dashboard stays mostly empty.

## Alerting

seadex-scout ships no notifier of its own, and its operational state is in its logs. Ship the container's logs to Loki and evaluate the rules in [`alerts/logql.yaml`](../alerts/logql.yaml) with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/). Grafana Alloy's Docker log discovery ships them with no extra configuration. Firing alerts go through your Alertmanager like any Prometheus alert. The rules cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `SeadexScoutCycleError` | a run logs an error that is not a lasting upstream or library problem, such as a failed Sonarr or Radarr library read | warning |
| `SeadexScoutUpstreamUnavailable` | SeaDex, AniList or the anime ID map has been failing for hours, and the existing findings are kept until it recovers | warning |
| `SeadexScoutLibraryDegraded` | Sonarr or Radarr keeps returning a much smaller or incomplete library | warning |
| `SeadexScoutScanStalled` | no `tick` or `cycle` completion line and no `reconcile started` in 3h, so the check loop has stopped | warning |
| `SeadexScoutReconcileStalled` | no `reconcile complete` in 72h, so the daily full pass has stopped while quick checks keep the other stall rule quiet | warning |
| `SeadexScoutFeedNotPolled` | no arr has read one tracker's RSS feed in 6h, though one did in the last 7 days | warning |
| `SeadexScoutBetterReleaseFound` | SeaDex recommends a better release than the one on disk, or a newer revision of the group on disk | info |
| `SeadexScoutMixedGroupManual` | the files on disk span more than one release group, so the app cannot say which one you have | info |
| `SeadexScoutReportWritten` | a report run wrote a season-level report | info |

Route `SeadexScoutUpstreamUnavailable` and `SeadexScoutLibraryDegraded` to a receiver that sends resolved notifications, with a `repeat_interval` longer than an outage lasts. Each outage then sends one message when it starts and one when it ends. [Notes on each alert](#notes-on-each-alert) explains the routing for every rule.

The rules assume the default `log.level: info` and the JSON log format. Seven of the messages they match log at `info`. They are `tick complete`, `cycle complete`, `reconcile started`, `reconcile complete`, `report written`, `series spans multiple release groups, manual review` and `indexer request`. At `warn`, the report rule, the mixed-group rule and the feed rule never fire. Both stall rules then fire all the time on a healthy app, because only the `degraded` completion lines still log.

Drop those five rules before you raise the level, and expect the dashboard to stay mostly empty. For `log.format: text`, replace each `| json` stage with `| logfmt`, which reads the same fields from the text format.

The stall window assumes a `poll_interval` of 1h or less, and the default is 15m. For a longer interval, widen the window to at least three times the interval. The error rule and the two lasting-problem rules use a 1h window, which assumes a `poll_interval` of 20m or less. Above that, widen their windows to at least three times the interval too.

With `poll_interval: off`, each check runs as a `docker exec` child, so its lines never reach the container's log. The count rules then see nothing and both stall rules fire. Drop them and alert on your scheduler's job result instead, or point their selector at your runner's log stream if your collector ships it. The stall rules also fire for a container in `mode: report`, which runs no check loop. A report is visible to these rules only when it runs as the container's command, with `mode: report`.

Thresholds and the `severity` labels are starting points. Change the `container` selector to the label your log collector sets, such as `job` or `service`. Route by whatever labels your Alertmanager uses.

### Log contract

The messages listed at the top of [`alerts/logql.yaml`](../alerts/logql.yaml) are the log contract. The rules and the dashboard match only these messages. Each `attrs:` line names the attributes that are stable on its message, and `msg` and `level` are stable on every line. A listed message is not renamed or retyped without a matching change to that list.

Every other message and attribute seadex-scout logs is diagnostic. It may be reworded, change level or change type in any release. If you build a rule or a dashboard on one, keep your own copy of the text, because it may not survive an upgrade.

### Notes on each alert

#### `SeadexScoutCycleError`

This rule covers every `ERROR` line without a `condition` attribute. The usual cause is a failed read of the Sonarr or Radarr library, logged as `library walk failed`. Findings stop updating, and the container is marked unhealthy until a read works again. Check the arr `url` and `api_key`, that the arr is running, and that the `/config` volume is writable.

The other causes are also fixed on your side. The Torznab feed may have failed to start or crashed, for example because port `9118` is in use. Prowlarr may have rejected `indexer.prowlarr_api_key` or a Torznab URL. `state.json`, `overrides.json` or the health marker in `/tmp` may not be readable or writable.

A recovered crash, logged as `panicked`, is a bug in seadex-scout. Restart the container and report it with the log line. A check loop that stalled or could not take its lock in `/config` usually clears with a restart. Report it if it comes back.

A lasting problem carries a `condition` attribute and fires `SeadexScoutUpstreamUnavailable` or `SeadexScoutLibraryDegraded` instead, so an upstream outage never fires this rule once per pass. Three cases log at `warn` and do not reach this rule either. They are a run interrupted by shutdown, a state save skipped to keep data it could not classify, and a report skipped because another report holds the lock.

The 1h window is longer than the 15 minutes between two passes at the default `poll_interval`. A fault that repeats on every pass stays one firing alert, and a one-off error clears an hour later. Your Alertmanager's `repeat_interval` decides how often a firing alert is sent again. Clearing only means the error stopped repeating, not that something fixed it. Route this rule to a receiver with `send_resolved: false`.

#### `SeadexScoutUpstreamUnavailable` and `SeadexScoutLibraryDegraded`

Both rules watch a lasting state. While a problem holds, seadex-scout logs an `ERROR` line naming it in a `condition` attribute on every pass. A pass that checks the upstream logs the escalated diagnostic, and every other pass logs `degraded condition still standing`. The alert stays firing for the whole outage and resolves within an hour of recovery. seadex-scout saves its lasting problems in `state.json` and states each one again when it starts, so a restart during an outage keeps the alert firing.

Route both to a receiver with `send_resolved: true`, so the resolved message tells you the problem is over. Alertmanager also sends a firing alert again every `repeat_interval`, 4h by default. For one message per outage, set a `repeat_interval` longer than an outage lasts. Keep it below the notification-log retention that the `SeadexScoutBetterReleaseFound` notes describe.

Loki's ruler does not support `keep_firing_for`. Its rule definition has no such field, so the key parses and the value is discarded. The 1h lookback alone holds the alert between two log lines, and it covers four passes at the default 15-minute `poll_interval`. With `poll_interval: off`, each `poll` run is one pass, so make the window at least three times your scheduler's interval. A `poll` run started with `docker exec` logs to the exec session, so point the selector at the stream that carries its output.

`SeadexScoutUpstreamUnavailable` groups by `condition` only. Every other attribute on these lines, such as the streak count or the error text, changes between passes. Grouping by one would open a new alert on every pass. `SeadexScoutLibraryDegraded` adds `arr`, because a shrunken library is judged per arr. Each arr is then its own alert, and one arr recovering resolves only its own.

Each condition of `SeadexScoutUpstreamUnavailable` has more detail than its message holds:

- `seadex-unreachable` keeps your findings, and the Torznab feed keeps serving what it already has. Neither picks up anything new until SeaDex answers again. The alert then resolves within an hour.
- `seadex-catalogue-fetch-failing` also stops your library being checked against the whole catalogue. If SeaDex is unreachable at the same time, this is the same outage. The alert resolves after the next daily pass that downloads the catalogue, up to a day after SeaDex comes back.
- `seadex-window-oversize` means that for about 2 hours, SeaDex has reported more changes in the last 48 hours than the quick check downloads at once. Until it clears, only the daily full pass picks up changes. It usually follows a large edit at SeaDex, and a container clock running behind has the same effect.
- `anilist-lookups-failing` concerns title lookups at `graphql.anilist.co`. Anime matched by ID are not affected. The alert resolves after a daily pass whose lookups succeed.
- `mapping-refresh-rejected` concerns the anime ID map, the Fribb anime-lists file on GitHub. Matching continues on slightly older ID data, and on a first start with no saved copy, nothing is compared until an update is accepted. After you remove `state.json`, the next start rebuilds the caches, which can take about half an hour. The alert resolves once an update is accepted.

Each condition of `SeadexScoutLibraryDegraded` works like this:

- `library-walk-shrunk` means the arr returned less than half the series or movies it returned before. seadex-scout treats that as a fault and keeps comparing against its previous copy of that library. To accept a smaller library now, remove `state.json` from the `/config` volume and restart. Otherwise seadex-scout accepts it after 6 daily passes in a row, and the findings for the missing items and the alert then resolve.
- `library-walk-partial` keeps the existing findings of the affected series without refreshing them. The `sonarr episode fetch` lines in the log name those series.

#### `SeadexScoutScanStalled`

This rule is a deadman for the check loop in built-in mode. Every loop iteration ends with a line. `tick complete` closes a quick pass over recent changes, including one with nothing to do, and `cycle complete` closes a full pass. `tick degraded` and `cycle degraded` close a pass that a failed arr read, an upstream outage or a safety check stopped short. `reconcile started` marks the start of a full pass.

The rule counts the degraded lines too, so a long arr or upstream outage does not read as a dead loop. A restart would not fix an outage anyway. The failed read fires `SeadexScoutCycleError`, and an outage fires `SeadexScoutUpstreamUnavailable` after about 2 hours.

The rule matches `reconcile started` because every other line it matches marks the end of a pass. Without it, the rule would see nothing between a container start and the end of the first full pass. A slow cold pass could then fire it. A cold full pass takes about 25 minutes and has taken up to 2 hours. A pass that starts and never finishes still fires the rule.

The 3h window is the same allowance the health check gives a cold pass, so the two checks agree on what is healthy. At the default 15m interval, the container turns unhealthy at 3h too. If something restarts unhealthy containers, this rule firing soon after means the restart did not help. Route it to a receiver with `send_resolved: true`, so you are told when the loop recovers.

The rule cannot tell why the log went quiet. A stopped or renamed container, or a log pipeline that stopped shipping, fires it too.

#### `SeadexScoutReconcileStalled`

This rule watches the daily full pass alone. Most loop iterations are quick checks over a bounded window of recent SeaDex changes, and their `tick complete` lines keep the stall rule satisfied. That window cannot see a deletion, a de-curation, an in-place torrent edit, the other entries that share a torrent, an outage longer than the window, or a clock wrong by more than it. The full pass catches all of these. It also refreshes the copy of your Sonarr and Radarr libraries that quick checks compare against, so while it is stopped, a release you already upgraded keeps being reported.

The full pass runs every 24h over the whole catalogue and the whole arr library, and it rebuilds the whole feed and search index. It logs `reconcile complete` even when it ends degraded, because it still did all of that work, and the `cycle degraded` line beside it carries the quality signal. No `reconcile complete` in 72h means the full pass keeps failing or the loop's schedule is wrong. The 72h window is three reconcile intervals, so two missed passes are tolerated and a third fires the rule. The `poll_interval: off` caveat of the stall rule applies here too.

#### `SeadexScoutFeedNotPolled`

Each arr polls an enabled Torznab indexer on its own RSS interval, and every poll logs an `indexer request` line with `feed=true`. An arr that marks the indexer failed, or an indexer disabled by hand, stops polling. seadex-scout logs no error for it, and new SeaDex releases stop reaching the arr. Open the indexer in Prowlarr, Sonarr or Radarr, check whether it is disabled or marked as failing, and run its test.

The rule fires for each tracker, `nyaa` or `ab`, that was polled in the last 7 days and has been silent for 6 hours. A tracker you keep disabled never fires. The alert resolves at the first evaluation after the next poll. It also resolves on its own once a tracker has been silent for the whole 7 days, which counts as disabled on purpose. The 6-hour window is several times the usual RSS interval of 15 to 60 minutes, so raise it if your arrs poll less often.

Route it to a receiver with `send_resolved: true`. It also fires when seadex-scout itself is down or its logs stop reaching Loki. `SeadexScoutScanStalled` then fires beside it and names the cause.

#### `SeadexScoutBetterReleaseFound`

This rule is an announcement and the app's activity signal. Keep it at `info` or drop it, because it does not need to page anyone. seadex-scout logs every open finding again on every pass, so the alert stays firing until you upgrade the release or add the show to `filters.ignore`. A notification lost on its way to your receiver is therefore sent at the next evaluation. Keep `send_resolved: false`, because a resolved message after you download the release tells you nothing new.

Each alert names one title with clickable links, because the rule groups by the finding's labels. `nyaa_url` only ever holds a Nyaa link. A public release from another tracker, such as AnimeTosho or RuTracker, arrives as `public_url` named by `public_tracker`, so a finding carries one public link at most. `ab_url` holds an AnimeBytes link, or a link that might be one, because a doubtful link is treated as private. `ab_tracker` names the link's own tracker, so a link labelled AnimeBytes whose URL is a public tracker page shows under that tracker's name.

`info_hash` is in the grouping so that a new encode by the same group is a new announcement. A release only on a private tracker publishes no info hash, so that one change cannot be told apart. `seadex_tags` covers that gap. seadex-scout builds it from the recommendation's status, kind, resolution and dual audio, such as `best · remux · 1080p · dual-audio`, so it moves only when the recommendation really changes. On an AnimeBytes-only release, a move from a 1080p encode to a 2160p remux therefore still fires a new alert.

A newer revision of the group you have shares the message, with `seadex_tags` starting `newer-revision · v2`, so it is its own announcement. The annotations can show `seadex_tags` only because it is in the grouping, since `sum by` drops every label it does not list. If a later version of seadex-scout classifies a release differently, that release is announced once more.

The 12h window must be longer than your `poll_interval`, with margin, because Loki's ruler has no `keep_firing_for`. Each pass logs every open finding again, so at the default 15m the window covers a long run of missed or silent passes. On a 3h interval it covers four. A window that is too tight makes the alert fire, resolve and fire again over a quiet stretch.

Group the route on the finding's identity, `alertname`, `al_id`, `season`, `alert_recommended_group` and `info_hash`, instead of inheriting a coarse `[alertname, severity]`. A group notification lists every alert in its group. Under a coarse grouping, one new release therefore sends every release you already handled again, unmarked.

Set a long `repeat_interval` so nothing comes back, and raise the notification-log retention with it. Alertmanager's notification log remembers that a group was notified, and its entries expire after `--data.retention`, 120h by default. On Mimir's built-in Alertmanager the flag is `-alertmanager.storage.retention`. Once an entry is gone, the still-firing alert reads as new and is sent again.

The effective repeat is therefore the shorter of `repeat_interval` and the retention. A one-year interval on a default install sends everything again every five days, and only a startup warning hints at it. The Alertmanager project describes this in [prometheus/alertmanager#2890](https://github.com/prometheus/alertmanager/issues/2890).

Not repeating has a cost, because a repeat is also a redelivery. A notification lost to an unreachable receiver, after Alertmanager's own retries, is not sent again. That is acceptable here, because the release is also in the Torznab feed, the logs and the report. If it is not acceptable for you, a monthly repeat still reads as "never" while giving a failed send another chance. A month is longer than 120h, so raise the retention for it too.

The links in the description render in Discord and Slack, so simplify the description for a plain-text receiver. The annotations use `alert_title` and `alert_recommended_group`, the markdown-safe copies of `title` and `recommended_group`. An untrusted SeaDex title then cannot become an active link or a code span, and the raw labels stay for search and grouping. Mention suppression is a setting of your receiver, such as an empty `allowed_mentions.parse` in a Discord webhook payload.

A receiver that echoes an alert's labels, for example in a footer, lists every label the rule groups by. To show only the tag line, render `seadex_tags` from your receiver's template instead.

#### `SeadexScoutMixedGroupManual`

This rule fires for a season or film whose files span more than one release group, so seadex-scout cannot name the one you have. Like a better release, it is logged again on every pass and uses the same 12h window. `arr` is in the grouping because one AniList entry can own an item in each arr. It does not cover a film or special filed in Sonarr's season 0. seadex-scout is silent for those on purpose, because that folder cannot tie a file to one entry, as the `unattributed` verdict in [How seadex-scout works](how-it-works.md) explains.

#### `SeadexScoutReportWritten`

This rule fires once per report run. It is a one-time event, so route it to a receiver with `send_resolved: false`, or it sends a resolved message that means nothing. The default selector sees a report only when it runs as the container's command, with `mode: report` or `report` as the container's argument. A report started with `docker exec` logs to the exec session, and a scheduler's `job-exec`, such as Ofelia's, keeps the output in the scheduler's own log. For those runs, point the selector at the runner's log stream or skip this rule.

An empty `markdown` field on the `report written` line means only the `.json` half of the pair was written. The JSON file was renamed into place, but the folder holding it could not be synced to disk. The run stopped there to keep the two files in order. The `.json` is complete, and running the report again writes a fresh pair.

# Monitoring and alerts

This page describes what seadex-scout logs, how its health check works, and the Loki alert rules it ships. Read it when you want alerts on findings or on a stalled app.

## Log lines

seadex-scout has no metrics endpoint. Its only output is its log, written as JSON to standard output. It opens no network port unless you configure the [indexer](torznab-indexer.md), which listens on the fixed port `9118`.

A finding is one line at `warn` with `msg="better release available"`. It carries the title, the AniList ID, the current and recommended groups, and the release's classification. It also carries one link per source you can get it from, as `nyaa_url`, `public_url` with `public_tracker`, or `ab_url` with `ab_tracker`, so an alert can show a clickable link straight from the labels. [`alerts/logql.yaml`](../alerts/logql.yaml) names the attributes it groups by. A `better_release` finding also carries `current_tier`. It is `alt` when SeaDex lists the group you have without recommending it, and `unlisted` when SeaDex does not list it at all.

A newer revision of a group you already hold logs the same `warn` message with `status=newer_revision`, `current_revision` and `recommended_revision`, such as `v1` and `v2`. Its `seadex_tags` value starts with `newer-revision`, so existing better-release alert rules fire on it too.

Informational cases log at `info`. Their statuses are `incomplete`, `theoretical_best`, `mixed_group_manual` and `unverifiable`.

Every pass ends with a completion line. A healthy pass logs `tick complete` or `cycle complete`. A pass that ran but could not compare logs `tick degraded` or `cycle degraded` at `warn`, with a `reason`, when an upstream outage or a safety check skipped the comparison. Every full pass also logs `reconcile complete`. Report mode logs one `report item` line per anime.

Every full pass whose library read was complete also logs one `library summary` line. It carries the report's counts per verdict, `anime_items`, `items_with_entry`, `items_all_best` and `hidden_by_filters`. It also logs one `library gap` line per entry the report calls `no_file` or `unverified`. These lines use different messages from report mode's `report summary` and `report item`, so a count never adds the two together. The `attrs:` lines in [`alerts/logql.yaml`](../alerts/logql.yaml) list the stable attributes of each message.

A finding is logged again on every pass for as long as it is true, so an alert keeps firing until you upgrade the release. It stops when a later pass no longer finds it.

A problem that lasts, such as SeaDex being unreachable, logs an `error` line with a `condition` attribute on every pass until it clears. It saves these problems in its state, so a restart does not clear an alert that is still true.

## Health

The image's Docker `HEALTHCHECK` runs the `seadex-scout health` subcommand. It reads a marker file at `/tmp/.healthy`, so it needs no shell and no port. Each completed pass refreshes the marker, and the marker reflects whether the last read of your Sonarr or Radarr library worked.

A failed library read marks the container unhealthy, because a restart or a config fix can recover it. A SeaDex, mapping or AniList outage keeps the container healthy, because a restart cannot fix an outage somewhere else, and the existing findings stay in place.

The container also turns unhealthy when no pass finishes within three poll intervals, and never sooner than 3 hours. The log then says `no cycle has completed within the health lease; marking unhealthy`. With `poll_interval: off` this check is off, because an outside scheduler runs the passes.

## Dashboard

Every release attaches `grafana-dashboard.json` to its GitHub Release, with a `.sha256` file and a signature. The same file is published as `ghcr.io/cplieger/seadex-scout/dashboard:<version>`. Import it into Grafana with **Dashboards**, then **New**, then **Import**. It reads Loki only.

The dashboard has two variables at the top. **Loki** picks your Loki data source. **Container** is the value of the `container` label your log collector puts on seadex-scout's lines, `seadex-scout` by default.

It is laid out in the order you use it:

- At a glance shows one status, the number of upgrades available, the findings to check by hand, the upgrades that are new in the selected time range, and the share of your anime already at SeaDex's best release.
- Act on these lists every upgrade with the time it was first seen, newest first. Each title opens the series or film in Sonarr or Radarr, and each row links the recommended release and the SeaDex entry. The Why column says how much each upgrade matters. A newer version is a fixed release of the group you already have, so it is worth taking. Optional means you hold a release SeaDex lists as an alternative. Your release is not on SeaDex means SeaDex does not list the group you have at all. The Format column shows the kind of release SeaDex recommends, such as `encode · 1080p · dual-audio`. If your Loki refuses the look-back over the selected range, the upgrades still show with First seen empty.
- What changed lists the upgrades that stopped being reported in the selected range. Usually the arr imported the recommended release.
- Your library against SeaDex shows how many anime you have, how many have a SeaDex entry and how many are already at SeaDex's best, one bar per verdict, a daily trend, and the entries that need a file or a manual look.
- Scout health is collapsed. It shows why the status is not Healthy, when the last checks finished, how often your arrs read the Torznab feed, and how many releases the feed newly offered.

The status reads Healthy, Upstream outage, Needs attention or Stalled. It is built from the same conditions as the shipped alert rules. Upstream outage means SeaDex, AniList or the anime ID map is failing, and nothing needs fixing on your side.

First seen is the earliest time a finding was logged within the selected time range. A finding older than the range, or older than the logs your Loki keeps, shows the earliest time still held. Widen the range to look further back. Loki's `max_query_length` setting can refuse a very long range. First seen needs a Loki version whose `label_format` supports `__timestamp__`. Loki 3.7 is known to work.

The library row is the SeaDex view, like the report. Your remux and dual-audio filters are not applied, so its numbers can disagree with Upgrades available. Hidden by your filters counts the entries SeaDex has a better release for that seadex-scout reports nothing about, mostly because your filters exclude that release or because you ignore the show. An anime whose only SeaDex entries have no file, or are offered in the feed without being compared, counts as having an entry and never as at best. The share at best then stays below 100 percent while any exist.

The row reads the `library summary` line each full daily check logs, so it lags by up to a day. A full check whose library read was incomplete logs no summary. The row then keeps the previous summary until it is 26 hours old and is empty after that, until a complete check logs a new one. It is also empty until the first full check after you upgrade to a version that logs it. The share at best is empty while no anime in your library has a SeaDex entry. A row in Library gaps that cleared can stay for up to 26 hours after the full check that last listed it.

With `poll_interval: off`, each check runs in a `docker exec` child whose lines never reach the container log. The dashboard then reads Stalled and stays mostly empty.

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

Route `SeadexScoutUpstreamUnavailable` and `SeadexScoutLibraryDegraded` to a receiver that sends resolved notifications, with a `repeat_interval` longer than an outage lasts. Each outage then sends one message when it starts and one when it ends. The comments in `alerts/logql.yaml` explain the routing for each rule.

The rules assume the default `log.level: info` and the JSON log format. At `warn`, the report rule and the mixed-group rule never fire, and both stall rules fire all the time on a healthy app. For `log.format: text`, replace each `| json` stage with `| logfmt`.

The stall window assumes a `poll_interval` of 1h or less, and the default is 15m. For a longer interval, widen the window to at least three times the interval. The error rule and the two lasting-problem rules use a 1h window, which assumes a `poll_interval` of 20m or less. Above that, widen their windows to at least three times the interval too.

With `poll_interval: off`, each check runs as a `docker exec` child, so its lines never reach the container's log. The count rules then see nothing and both stall rules fire. Drop them and alert on your scheduler's job result instead. A report is visible to these rules only when it runs as the container's command, with `mode: report`.

Thresholds and the `severity` labels are starting points. Change the `container` selector to the label your log collector sets, such as `job` or `service`. Route by whatever labels your Alertmanager uses.

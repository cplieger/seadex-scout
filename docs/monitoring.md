# Monitoring and alerts

This page describes what seadex-scout logs, how its health check works, and the Loki alert rules it ships. Read it when you want alerts on findings or on a stalled app.

## Log lines

seadex-scout has no metrics endpoint. Its only output is its log, written as JSON to standard output. It opens no network port unless you configure the [indexer](torznab-indexer.md), which listens on the fixed port `9118`.

A finding is one line at `warn` with `msg="better release available"`. It carries the title, the AniList ID, the current and recommended groups, and the release's classification. It also carries one link per source you can get it from, as `nyaa_url`, `public_url` with `public_tracker`, or `ab_url` with `ab_tracker`, so an alert can show a clickable link straight from the labels. [`alerts/logql.yaml`](../alerts/logql.yaml) names the attributes it groups by.

A newer revision of a group you already hold logs the same `warn` message with `status=newer_revision`, `current_revision` and `recommended_revision`, such as `v1` and `v2`. Its `seadex_tags` value starts with `newer-revision`, so existing better-release alert rules fire on it too.

Informational cases log at `info`. Their statuses are `incomplete`, `theoretical_best`, `mixed_group_manual` and `unverifiable`.

Every pass ends with a completion line. A healthy pass logs `tick complete` or `cycle complete`. A pass that ran but could not compare logs `tick degraded` or `cycle degraded` at `warn`, with a `reason`, when an upstream outage or a safety check skipped the comparison. Every full pass also logs `reconcile complete`. Report mode logs one `report item` line per anime.

A finding is logged again on every pass for as long as it is true, so an alert keeps firing until you upgrade the release. It stops when a later pass no longer finds it.

A problem that lasts, such as SeaDex being unreachable, logs an `error` line with a `condition` attribute on every pass until it clears. It saves these problems in its state, so a restart does not clear an alert that is still true.

## Health

The image's Docker `HEALTHCHECK` runs the `seadex-scout health` subcommand. It reads a marker file at `/tmp/.healthy`, so it needs no shell and no port. Each completed pass refreshes the marker, and the marker reflects whether the last read of your Sonarr or Radarr library worked.

A failed library read marks the container unhealthy, because a restart or a config fix can recover it. A SeaDex, mapping or AniList outage keeps the container healthy, because a restart cannot fix an outage somewhere else, and the existing findings stay in place.

The container also turns unhealthy when no pass finishes within three poll intervals, and never sooner than 3 hours. The log then says `no cycle has completed within the health lease; marking unhealthy`. With `poll_interval: off` this check is off, because an outside scheduler runs the passes.

## Alerting

seadex-scout ships no notifier of its own, and its operational state is in its logs. Ship the container's logs to Loki and evaluate the rules in [`alerts/logql.yaml`](../alerts/logql.yaml) with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/). Grafana Alloy's Docker log discovery ships them with no extra configuration. Firing alerts go through your Alertmanager like any Prometheus alert. The rules cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `SeadexScoutCycleError` | a run logs an error that is not a lasting upstream or library problem, such as a failed Sonarr or Radarr library read | warning |
| `SeadexScoutUpstreamUnavailable` | SeaDex, AniList or the anime ID map has been failing for hours, and the existing findings are kept until it recovers | warning |
| `SeadexScoutLibraryDegraded` | Sonarr or Radarr keeps returning a much smaller or incomplete library | warning |
| `SeadexScoutScanStalled` | no `tick` or `cycle` completion line and no `reconcile started` in 3h, so the check loop has stopped | warning |
| `SeadexScoutReconcileStalled` | no `reconcile complete` in 72h, so the daily full pass has stopped while quick checks keep the other stall rule quiet | warning |
| `SeadexScoutBetterReleaseFound` | SeaDex recommends a better release than the one on disk, or a newer revision of the group on disk | info |
| `SeadexScoutMixedGroupManual` | the files on disk span more than one release group, so the app cannot say which one you have | info |
| `SeadexScoutReportWritten` | a report run wrote a season-level report | info |

Route `SeadexScoutUpstreamUnavailable` and `SeadexScoutLibraryDegraded` to a receiver that sends resolved notifications, with a `repeat_interval` longer than an outage lasts. Each outage then sends one message when it starts and one when it ends. The comments in `alerts/logql.yaml` explain the routing for each rule.

The rules assume the default `log.level: info` and the JSON log format. At `warn`, the report rule and the mixed-group rule never fire, and both stall rules fire all the time on a healthy app. For `log.format: text`, replace each `| json` stage with `| logfmt`.

The stall window assumes a `poll_interval` of 1h or less, and the default is 15m. For a longer interval, widen the window to at least three times the interval. The error rule and the two lasting-problem rules use a 1h window, which assumes a `poll_interval` of 20m or less. Above that, widen their windows to at least three times the interval too.

With `poll_interval: off`, each check runs as a `docker exec` child, so its lines never reach the container's log. The count rules then see nothing and both stall rules fire. Drop them and alert on your scheduler's job result instead. A report is visible to these rules only when it runs as the container's command, with `mode: report`.

Thresholds and the `severity` labels are starting points. Change the `container` selector to the label your log collector sets, such as `job` or `service`. Route by whatever labels your Alertmanager uses.

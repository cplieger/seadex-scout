# Configuration

This page lists every seadex-scout setting and shows how to run its checks from an outside scheduler. The README's [Configuration reference](../README.md#configuration-reference) covers the settings most people change.

## Where settings live

All configuration lives in one YAML file, `/config/config.yaml`. The first start writes it from the annotated template [`config.example.yaml`](../config.example.yaml) with a generated `feed_api_key`. seadex-scout reads the file once at start, so restart the container after an edit.

With no connection variable set, the first start still writes the file and then stops with a message naming both fixes. Set the variables, or open the file and put the `url` and `api_key` values in it, then restart.

An unknown or misplaced key stops the start with an error that names it, such as `unknown configuration key "anime_bytes"`. A typo fails at once instead of being silently ignored.

The addresses of SeaDex, animap and AniList, how often they are asked, and the file locations under `/config` are fixed. They are not config keys, so the file holds only what you tune.

## Environment variables

Any string value in the file can name a `SONARR_*`, `RADARR_*` or `SEADEX_SCOUT_*` environment variable as `${VAR}`, so secrets can stay in an `.env` file instead of the file. The file is the source of truth. A variable is read only where the file names it, and the starter file does that for the four connection values below.

A connection value that names an unset variable reads as empty, so Radarr stays off until `RADARR_URL` is set. Anywhere else, a reference to an unset variable stays as written. API keys are never logged, only whether each one is set.

| Variable | Description | Default |
| --- | --- | --- |
| `SONARR_URL` | Where seadex-scout reaches Sonarr. Unset turns Sonarr off. | _(unset)_ |
| `SONARR_API_KEY` | Sonarr's API key, needed when `SONARR_URL` is set. | _(unset)_ |
| `RADARR_URL` | Where seadex-scout reaches Radarr. Unset turns Radarr off. | _(unset)_ |
| `RADARR_API_KEY` | Radarr's API key, needed when `RADARR_URL` is set. | _(unset)_ |
| `CONFIG_PATH` | Path of the config file. | `/config/config.yaml` |

The example compose file also passes `SEADEX_SCOUT_FEED_KEY`, `SEADEX_SCOUT_PROWLARR_KEY` and `SEADEX_SCOUT_AB_PASSKEY`. They are read only where `config.yaml` names them, for the `indexer.feed_api_key`, `indexer.prowlarr_api_key` and `indexer.ab_passkey` keys.

## Every key

Defaults are the values the starter file ships.

| Key | Default | Description |
| --- | --- | --- |
| `sonarr.url` | `${SONARR_URL}` | Where seadex-scout reaches Sonarr. Sonarr is on when this is set. At least one of Sonarr and Radarr must be on. |
| `sonarr.api_key` | `${SONARR_API_KEY}` | Required when Sonarr is on. |
| `sonarr.enabled` | _(unset)_ | `false` turns Sonarr off whatever `url` says. `true` requires `url` and `api_key`. Absent follows `url`. |
| `sonarr.public_url` | _(unset)_ | The Sonarr address your browser uses, for links in the report. Empty reuses `sonarr.url`. |
| `radarr.*` | `${RADARR_URL}`, `${RADARR_API_KEY}` | The same four keys as `sonarr`. |
| `mode` | `daemon` | `daemon` runs on a schedule. `report` writes one report and exits. |
| `poll_interval` | `15m` | How often to check SeaDex for changes. Minimum `15m`. `off` hands scheduling to an outside tool. |
| `animebytes` | `false` | Set `true` when you have an AnimeBytes account, to include its releases and links. |
| `filters.exclude_remux` | `false` | Leave out releases classified as remux. |
| `filters.require_dual_audio` | `false` | Leave out releases that are not dual audio. |
| `filters.exclude_specials` | `false` | Leave out OVA, ONA and special entries. |
| `filters.exclude_tags` | `{}` | SeaDex tags to drop, each listing the places to drop it from, out of `findings`, `report` and `feed`. |
| `filters.ignore` | `[]` | AniList IDs of shows to stop warning about. They still appear in the report and the indexer. |
| `arr_tags.include` | `[]` | Check only items with one of these Sonarr or Radarr tags. `[]` checks everything. |
| `arr_tags.exclude` | `[]` | Never check items with one of these tags. An exclude wins over an include. |
| `report.dir` | `/config/reports` | Where each report pair is written. |
| `indexer.feed_api_key` | _(generated on first start)_ | The key Sonarr and Radarr send and the feed checks. No spaces and no `$`. |
| `indexer.nyaa_torznab_url` | _(unset)_ | Prowlarr's Nyaa Torznab URL, such as `http://192.0.2.10:9696/1/api`. Empty turns Nyaa off. |
| `indexer.ab_torznab_url` | _(unset)_ | Prowlarr's AnimeBytes Torznab URL. Empty turns AnimeBytes off. |
| `indexer.prowlarr_api_key` | _(unset)_ | Prowlarr's API key. Secret, never logged. |
| `indexer.ab_passkey` | _(unset)_ | AnimeBytes passkey for the AnimeBytes RSS download links, 32, 48 or 56 characters. Empty turns AnimeBytes RSS off. Nyaa needs none. |
| `log.level` | `info` | `debug`, `info`, `warn` or `error`. The shipped alert rules need `info`. |
| `log.format` | `json` | `json` or `text`. |

### API key checks

Sonarr, Radarr and Prowlarr generate a 32-character hex API key. A key of any other shape is accepted with a startup warning naming the field, because each of them honours whatever key you set. A key that contains a `$` is refused, because it is a `${VAR}` reference that was never filled in. An empty `indexer.prowlarr_api_key` is valid when Prowlarr's authentication is off for local addresses.

### Tag filters

`filters.exclude_tags` is empty by default, which filters nothing. A release SeaDex tagged `Broken` is then still logged as a better release, still counts as a best in the report, and is still served by the indexer. The report marks it `(broken)` either way. To drop a tag, list it with the places to drop it from:

```yaml
filters:
  exclude_tags:
    broken: [findings, report, feed]
    incomplete: [feed]
```

Tag matching is exact and ignores case. An unknown place name, or a tag with no places, stops the start with an error.

## Scheduling

By default, seadex-scout runs its own schedule. `poll_interval` sets how often it checks SeaDex for changes, `15m` by default and at least `15m`. Once a day it also runs a full pass that rereads the whole SeaDex list and your library. That daily pass is fixed and is not a setting. [How seadex-scout works](how-it-works.md#checks-and-full-passes) explains the two kinds of pass.

To run each check from an outside scheduler instead, set `poll_interval: off`, or `disabled`, or `0`. The container then idles and stays healthy, and the scheduler runs the `poll` subcommand for each check. Each `poll` runs one check, updates the health marker, and exits with `0` or `1`.

Each `poll` is a separate process that starts with no cached library, so every `poll` is a full pass. Schedule it about 24 hours apart, not every few minutes. The indexer serves the result of the last check, so its RSS list is empty until the first `poll` runs. With [Ofelia](https://github.com/mcuadros/ofelia), label the service:

```yaml
    labels:
      ofelia.enabled: "true"
      ofelia.job-exec.seadex-poll.schedule: "@every 24h"
      ofelia.job-exec.seadex-poll.command: "/seadex-scout poll"
```

Any scheduler works. The whole contract is `docker exec seadex-scout /seadex-scout poll`.

To write reports on a schedule, use the same Ofelia `job-exec` pattern with `/seadex-scout report`. A report started while another is still running logs `report skipped; another report is already running` and exits with `0`, so an overlap is not a failure.

In this mode each check's log lines go to the scheduler, not to the container's log, which changes what the alert rules can see. [Monitoring and alerts](monitoring.md#alerting) explains what to do instead.

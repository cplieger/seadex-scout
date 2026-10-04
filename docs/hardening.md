# Security

This page covers how seadex-scout handles its credentials and how to run it with a hardened compose setup. The README's [Security](../README.md#security) section has the short version.

## What the indexer exposes

seadex-scout opens no network port until you configure the [indexer](torznab-indexer.md). The indexer then listens on port `9118`. It checks every request against `feed_api_key`, and a request without the matching `apikey` gets `401`.

Its links are Prowlarr proxy links for searches. For the AnimeBytes RSS feed, they are direct AnimeBytes links that embed your `ab_passkey`. Treat the endpoint as sensitive and keep it on your local network. An internal reverse proxy in front of it is fine, and the per-tracker subdomains in the [setup guide](torznab-indexer.md#per-tracker-search-gating) exist for that. Do not put it on the public internet.

## Credentials on disk and in logs

seadex-scout sends the Prowlarr API key in a request header, never in a logged URL, and never writes it to the logs. Sonarr and Radarr keys are never logged either, only whether each is set.

The indexer saves its feed between checks as `/config/feed.json`. Each write stores the items without their download links, and the indexer rebuilds the AnimeBytes links from your current `ab_passkey` when it serves them. The file is readable only by its owner, with mode `0600`. A `feed.json` written by an earlier version can still hold passkey links until the next feed write replaces it. A backup of `/config` carries the passkey when `config.yaml` holds it directly rather than through `${SEADEX_SCOUT_AB_PASSKEY}`.

## Hardened compose setup

The image is distroless, with no shell, and runs as a non-root user. For a hardened setup, add these lines to the service in `compose.yaml`:

```yaml
    read_only: true
    cap_drop: ["ALL"]
    security_opt: ["no-new-privileges:true"]
    tmpfs: ["/tmp:size=1m,mode=1777,noexec,nosuid,nodev"]  # holds the health marker
```

The small `/tmp` holds the health marker, which the read-only file system would otherwise block. `/config` stays writable through its volume.

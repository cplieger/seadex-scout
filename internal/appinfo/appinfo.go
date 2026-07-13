// Package appinfo holds seadex-scout's fixed identity constants shared across
// its outbound HTTP clients, so the app presents one consistent identity to
// every upstream (SeaDex, Fribb, AniList, Prowlarr) from a single source rather
// than each client redeclaring it.
package appinfo

// UserAgent is the User-Agent every seadex-scout outbound HTTP request sends. It
// is one fixed identity string, not a per-client setting, so all clients share
// this constant.
const UserAgent = "seadex-scout (+https://github.com/cplieger/seadex-scout)"

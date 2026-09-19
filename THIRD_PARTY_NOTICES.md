# Third-party notices

No third-party code is included in this repository. Two upstream projects are
followed in design only, so no license text is reproduced here.

Each is named at the line of ours that follows it:

- The job itself follows [bbtufty/seadexarr](https://github.com/bbtufty/seadexarr)
  (GPL-3.0): walk a Sonarr/Radarr anime library, resolve each title to its SeaDex
  entry by AniList ID, and report where SeaDex recommends a better release than
  the file on disk. Ours is the comparison in `internal/compare/compare.go:136`
  (`Comparer.Compare`), and the README's "The problem" section names the two gaps
  this project was written to close.
- The tracker-id extraction in `internal/indexer/match.go:281` (`extractID`, with
  the digits-only check in `validTrackerID` at line 295) follows
  [Ryder-C/seadexerr](https://github.com/Ryder-C/seadexerr): read the id out of a
  tracker's page URL and accept it only when every character is a digit. That
  repository declares no license, carrying no LICENSE file and no `license` field
  in its `Cargo.toml`, so nothing from it is reproduced here.

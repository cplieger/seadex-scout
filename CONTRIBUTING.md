# Contributing to seadex-scout

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Scope

- A built-in notifier, a chat or webhook output, a web findings page and a direct push to a download client are out of scope. The daemon reports findings only as log lines, and alerting is the log stack's job.
- A matching change is judged first by the false findings it adds. A mapping rule must hold for every title the sources describe. Code never special-cases one show, and an operator corrects a single title in `/config/overrides.json`.

## Rules

- When a field `state.json` persists changes type, give it a new JSON key and a test loading the old key. Retyped in place, the old key fails the decode, so the app moves the file aside and loses its state.
- The messages and attributes listed atop `alerts/logql.yaml` are a public contract. Add an attribute rather than rename one. Operators run copies of the rules and the dashboard, so a rename passes every test here and silences their alerts.
- A new config key needs its default and check in `internal/config`, an entry in `config.example.yaml` and a row in `docs/configuration.md`. A first start writes the embedded example, so a key missing there stays hidden from a new install.

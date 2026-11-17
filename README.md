# caldav-server

A very small CalDAV server. Calendars and events live in a local [Turso](https://github.com/tursodatabase/turso) database (SQLite-compatible, in-process).

- Build: `go build` (or `nix build`)
- Run: `ADDR=127.0.0.1:5232 DATA_DIR=./data ./caldav`
- Test: `go test -race ./...` (~1.5 s); hot-path benchmarks: `go test -run x -bench . ./...`
- Data: `DATA_DIR/caldav.db` (plus its `-wal` file). A calendar named `calendar` is created per user.
- The Turso driver extracts its native library into `$TURSO_GO_CACHE_DIR` (default: the user cache dir, e.g. `~/.cache/turso-go`). That directory must be writable — set `TURSO_GO_CACHE_DIR` for services without a home directory.
- Auth: none built in. Run it on loopback behind a reverse proxy that does auth and sets `X-Remote-User`.
- Not implemented: sync-collection, time-range filtering (all objects are returned), PROPPATCH (refused).

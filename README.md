# caldav-server

A very small CalDAV server: one `.ics` file per event, standard library only, no database.

- Build: `go build` (or `nix build`)
- Run: `ADDR=127.0.0.1:5232 DATA_DIR=./data ./caldav`
- Data: `DATA_DIR/<user>/<calendar>/<id>.ics`; a calendar named `calendar` is created per user.
- Auth: none built in. Run it on loopback behind a reverse proxy that does auth and sets `X-Remote-User`.
- Not implemented: sync-collection, time-range filtering (all objects are returned), PROPPATCH (refused).

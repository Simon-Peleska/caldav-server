# caldav-server

A very small CalDAV server. Calendars and events live in a local [Turso](https://github.com/tursodatabase/turso) database (SQLite-compatible, in-process).

- Build: `go build` (or `nix build`)
- Run: `ADDR=127.0.0.1:5232 DATA_DIR=./data ./caldav`
- Test: `go test -race ./...` (~1.5 s); hot-path benchmarks: `go test -run x -bench . ./...`
- Data: `DATA_DIR/caldav.db` (plus its `-wal` file). A calendar named `calendar` is created per user.
- The Turso driver extracts its native library into `$TURSO_GO_CACHE_DIR` (default: the user cache dir, e.g. `~/.cache/turso-go`). That directory must be writable — set `TURSO_GO_CACHE_DIR` for services without a home directory.
- Auth: none built in. Run it on loopback behind a reverse proxy that does auth and sets `X-Remote-User`.
- Web UI: open the server root in a browser (it redirects to `/~/`). Lists, adds and deletes the events of the `calendar` calendar (other calendars are CalDAV-only); the list refreshes within 10 s when a client changes the calendar.
- Notifications (Web Push): turn them on per device in the web UI's "Notifications" card. Event alarms (VALARM, or the event start without one) from all of the user's calendars go to all of their devices. On iPhone/iPad, add the page to the home screen first and turn them on from there. Optional `PUSH_SUBJECT=mailto:you@example.com` sets the contact sent to push services; by default it is the site's own https URL.
- Notification API: `curl -u user:pass -d title=Backup -d body=done -d url=/~/ https://host/~/api/notify` (or a JSON body `{"title", "body", "url"}`) sends to all devices of the authenticated user. The JSON answer lists `sent`, `failed` and each device's result; status 200 if any device got it, 502 if none did, 409 `E-API-NO-DEVICES` if none are set up.
- Not implemented: sync-collection, time-range filtering (all objects are returned), PROPPATCH (refused).

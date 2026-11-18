# Architecture

One HTTP handler, one Turso database file. `main.go` is the CalDAV server, `ui.go` the web UI.

## Web UI

Server-rendered `html/template` pages under `/~/` (`~` can't start a user name, so they never collide with CalDAV paths), made interactive with htmx 4, which is vendored and embedded (`htmx-4.0.1.min.js`). Same auth as CalDAV: the proxy's `X-Remote-User`. It shows only the default `calendar` (`uiCal`); other calendars stay CalDAV-only, since one is all the UI needs.

- Forms post with htmx; the handler writes the event (same transaction + `bumpCtag` as PUT) and returns the re-rendered `#events` section.
- Live updates without websockets: `#events` polls every 10 s with the ctag it was rendered from. The server answers 204 (htmx doesn't swap) if the ctag is unchanged, so an idle tab costs one point lookup per 10 s.
- Errors: `uiError` logs the code and returns it with `HX-Retarget: #error`, so htmx shows it in the error box whatever element made the request. Add and delete responses clear the box with an out-of-band swap; polls don't, so a background sync can't wipe an error the user is reading.
- CSRF: basic-auth credentials ride along on cross-site requests, so every UI request goes through `http.CrossOriginProtection` (Sec-Fetch-Site / Origin).
- Event display parses only SUMMARY / DTSTART / DTEND / DUE of the first VEVENT or VTODO. Times are shown as written in the event, with their TZID, since the server doesn't know the viewer's zone. Recurrences aren't expanded: a repeating event is listed once, never counted as past, under its first date if that is still ahead, otherwise in a "Repeating" group after the dated events. `DURATION` is ignored (such events count as past once they start) and completed tasks are listed like open ones. Events added in the UI use floating local times (or all-day dates).
- "Past" is decided at render time, and polls only re-render when the ctag changes, so an event that ends while the tab is open stays in the upcoming list until the next change or reload. Good enough for a simple UI.
- Cost (`bench_test.go`, 2000 events): unchanged poll 0.014 ms; full page 23 ms, mostly reading event rows from Turso, then templating (the day-grouped markup put it up from 17 ms) and ICS parsing. The UI write handlers release `server.mu` before re-rendering, so CalDAV writes don't wait on that.

## Storage

`DATA_DIR/caldav.db`, opened through `database/sql` with the `turso.tech/database/tursogo` driver (no CGO; it loads a Rust library via purego).

- `calendars(user, name, ctag)` — `ctag` is bumped (to `max(ctag+1, now_ns)`) in the same transaction as every change to the calendar's objects, so clients polling `getctag` see changes with a single point lookup. Nanoseconds so that a deleted and recreated calendar never reuses an old ctag.
- `objects(user, cal, name, etag, modified, data)` — `etag` (a hash of the data) is computed once on PUT and stored, so listings never hash event bodies.

Reads run concurrently over a pool of 8 connections. Writes are serialised by `server.mu`, which also makes If-Match / If-None-Match checks race-free.

## Performance

Measured over HTTP on loopback with a small load generator, 2000 events (~700 B each) in one calendar; old = one `.ics` file per event. `go test -bench .` (`bench_test.go`) covers the same read paths without the network.

| operation                           | old      | Turso    |
|-------------------------------------|----------|----------|
| PROPFIND depth 0 `getctag` (poll)   | 0.35 ms  | 0.02 ms  |
| PROPFIND depth 1 `getetag`          | 16 ms    | 8 ms     |
| calendar-query with data            | 19.5 ms  | 12.8 ms  |
| multiget 100 with data              | 1.1 ms   | 2.6 ms   |
| GET                                 | 0.010 ms | 0.019 ms |
| PUT                                 | 0.05 ms  | 0.7 ms   |

- Event bodies are only read from the database when `calendar-data` is asked for, so etag polls never touch them.
- Responses are built in one `bytes.Buffer`; calendar data is XML-escaped into it directly, without building a struct tree for `encoding/xml`.
- PUT is slower because the database fsyncs on commit; the old server renamed files without fsync and could lose writes on a crash.
- Every query costs ~15 µs of driver overhead (see GOTCHAS), which is what makes GET and multiget slower than reading a file. `ensureDefaultCalendar` therefore caches users in memory instead of querying on every request.
- Multiget does one point lookup per href: a single `name IN (...)` query was tried and was slower, because Turso seeks only on `(user, cal)` and scans the whole calendar.

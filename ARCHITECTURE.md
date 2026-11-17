# Architecture

One Go file (`main.go`), one HTTP handler, one Turso database file.

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

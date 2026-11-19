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

## Web Push (`push.go`)

Notifications go out over standard Web Push with no extra dependency: VAPID (RFC 8292, an ES256 JWT signed with `crypto/ecdsa`) and aes128gcm payload encryption (RFC 8291, `crypto/ecdh` + `crypto/hkdf` + AES-GCM). The VAPID key is created on first start and kept in the `settings` table, so it lives and dies with the database.

- Devices: `push_devices`, one row per subscription endpoint (id = hash of the endpoint), owned by a user. The page's `connect()` re-registers on every visit, so a renewed or replaced subscription (iOS does this) updates the row; `replaces` lets the page drop its old endpoint. The answer says whether the subscription is already `gone`, and the page then subscribes afresh. The page remembers its device id (per user), so a device removed from the list stays off instead of re-registering on the next visit; a subscription for a different server key is not taken as a removal. Error codes that only happen in the browser (permission, subscribing) are posted to `/~/push/log`, so they appear in the server log too.
- Endpoints: the server POSTs to whatever URL a device registered, so only https URLs on known push services (`pushServices`) are accepted, redirects are not followed, and only 401/403 response bodies (Apple's reason) are shown.
- Sending: `sendToUser` sends to all devices in parallel with a 10 s timeout each, so a dead device can't push the answer past the HTTP write timeout. Notifications carry no `tag`, so a second alarm for the same event isn't silently merged into the first.
- Alarms: `pushLoop` runs `notifyDue` every 30 s. It reads every event in all calendars of users with devices (not only the one the UI shows), computes alarm times from VALARM `TRIGGER`s (relative, `RELATED=END`, absolute) or the start (09:00 for all-day events), and sends those that fell due in the last 10 min. `push_sent` remembers what was sent so a restart doesn't repeat, and is cleaned after 48 h. Recurrences are not expanded.
- No silent failures, because push is fire-and-forget on the server side and silently dropped on iOS:
  - Every send stores `sent`, or the error code and message, on the device; the UI lists each device with that status.
  - 404/410 marks the device `gone` (skipped until it re-subscribes); 401/403 points at `PUSH_SUBJECT`, since Apple rejects bad JWT subjects.
  - The service worker always shows a notification (Safari revokes subscriptions that don't) and posts back to `/~/push/ack`, so "shown" proves the whole path worked, not just the push service's 201.
  - Test buttons send to one or all devices and show failures in place.
  - The page explains why it can't subscribe (iOS not installed to home screen, unsupported browser, permission denied) instead of hiding the button.
- API: `POST /~/api/notify` sends a message to all of the caller's devices and answers with per-device results. Same auth and same send path as alarms.
- Cost (`bench_test.go`, 2000 events): one alarm scan is ~9 ms every 30 s, mostly reading and parsing the events, the same as a page render.

## Coach (planned, not built)

What it must do is in `SPECIFICATION.md`. The design rests on one split:

- **Following the plan is deterministic Go.** This covers what's now, when to nudge and re-timing after an overrun. It runs in the server, needs no network and answers in milliseconds.
- **Changing the plan is Claude.** This covers understanding a brain dump or an email, and judgment calls such as "more park time?". Claude calls tools on the server; it never holds state of its own.

So when Claude is down, the day still runs, just less cleverly.

```
 iPhone Shortcuts ──signals──▶ POST /~/api/signal ─┐
 Action Button / Siri ─done──▶ POST /~/api/done  ──┤
 /~/now page (Done/Skip/+5) ───────────────────────┤
                                                   ▼
                                    ┌──────────── engine (Go) ────────────┐
 Claude Code / Claude app ──MCP──▶  │ plan_items · routines · places      │──▶ Web Push (push.go)
   (brain dump, shared email)       │ schedule() · tick every 30 s        │──▶ "plan" CalDAV calendar
                                    └───────┬─────────────────────────────┘
                                            │ decision points (left work, …)
                                            ▼
                                  claude -p job queue (one at a time,
                                  timeout, deterministic fallback)
```

### Data (new tables)

- **`plan_items(user, id, day, title, kind, state, fixed, priority, start, dur, est, place, parent, source, reason, done_at)`:**
  - `kind`: task, trip, appointment or free.
  - `state`: pending, active, done or skipped.
  - `fixed` items (appointments, work) are never moved by the scheduler.
  - `est` is the duration estimate; `dur` is what it actually took once done.
- **`routines(user, name, steps)`:** steps is a JSON array of `{title, est, optional, priority}`. The morning, the evening and "leave the house" are routines.
- **`durations(user, title, minutes, at)`:** each finished task adds a row. A task's estimate is the 75th percentile of its last 20 rows, so it leans long.
- **`places(user, name, lat, lon, radius)`:** home, work, the park. Trips are computed between places.
- **`travel(user, from, to, minutes, at)`:** measured trips, from leave and arrive signals.
- **`plan_log(user, at, actor, change, reason, undo)`:** every change made by the engine, Claude or the user, with a one-line reason and the inverse operation. This drives "why did this change?" and undo.

Appointments stay ordinary events in the user's calendars, so iOS Calendar, email invites and CalDAV keep working. The engine reads them (it already parses them for alarms) as fixed items, and doesn't copy them into `plan_items`.

### The scheduler

`schedule(user, now)` is one plain function that runs after every change.

1. **Gather:**
   - fixed items from the calendars;
   - flexible pending items from `plan_items`;
   - the active item.
2. **For each fixed item with a place different from the current one, insert in front of it:**
   - a trip: travel time from `travel`, else from the routing API, else a default;
   - a buffer: `max(5 min, 15% of the trip)`;
   - a "get ready to leave" step.

   The trip's start is the leave-by time.
3. **Lay out flexible items in order (priority, then insertion), starting at `max(now, active item's planned end)`, into the gaps before each leave-by.**
   - An item that doesn't fit moves to the next gap.
   - An optional item that fits nowhere today is marked skipped, and the change is logged with the reason "no time".
4. **Write the new start times** in one transaction, and log what moved.

It's a greedy fill, not an optimizer: a day has around 20 items, and a predictable plan beats an optimal one the user can't follow.

### The one current item

- **Choosing it:** the active item is the earliest pending item whose start ≤ now. When there is none, it's "free until HH:MM" (shown as such, so the screen is never empty).
- **Activating the next item:** only after the active one is confirmed (done, skipped or +5 min) or a signal ends it. Each activation sends one push: "Now: shower (15 min)".
- **Overrun (`tick`, every 30 s, next to `pushLoop`):**
  1. At the planned end: "Done with shower?" with Done / +5 actions.
  2. After another 5 min: a second nudge, and `schedule` re-runs, which shifts everything flexible.
  3. If a leave-by time is now at risk: the push says what gets dropped ("Skip breakfast, leave in 10 min").
- **`/~/now`** is one card: title, time left, and three big buttons. It is the home-screen app's start page. The day's list and the calendar are links below it.

### Inputs

- **`POST /~/api/signal {type, place?, at?}`**, called by Shortcuts automations. Types:
  - `woke` (Sleep "wake up" / alarm stopped): starts the morning routine at `at`.
  - `left`, `arrived` (location automations for saved places): confirm or end trips, record travel times, mark "leave for work" done.
  - `free` (manual): "I have time now".
- **`POST /~/api/done` / `/skip` / `/more`** act on the active item: the Action Button, Siri, and notification actions.
- **Auth:** same as everything else (the proxy's basic auth). Shortcuts' "Get contents of URL" can send the header.
- **Duplicates:** iOS location automations are flaky, firing late, twice or never. So signals are idempotent: a second identical signal within 10 min is ignored, and a stale `at` is honored rather than replaced by now.
- **Missing signals:** the user can always tap Done instead, and `/~/now` shows "Did you leave?" when a trip's leave-by time passes without a `left`.

### Claude

- **One MCP server, in the Go binary at `/~/mcp`** (streamable HTTP), with tools:
  - `now`, `get_day(date)`;
  - `add_appointment(title, start, end, place, notes)`: writes a VEVENT into the user's calendar;
  - `add_task(title, est, priority, day?, optional?)`;
  - `update_item`, `remove_item`;
  - `set_routine`, `get_preferences` / `set_preferences`;
  - `undo(log_id)`.

  Every tool requires a `reason` string, which goes into `plan_log`. The server re-runs `schedule` after each write, so Claude never computes times itself.
- **Brain dumps:** in a Claude Code session with this MCP server configured, or in the Claude app with it added as a custom connector.
- **Shared emails:** shared into the Claude app; Claude extracts the appointment and calls `add_appointment`.
- **Decision points** are moments where judgment helps: the `left` signal at work, the evening plan at a fixed time, and a free gap over 45 min.
  - The engine queues a job.
  - A worker runs `claude -p` with the MCP tools, a short prompt (the day, the weather, preferences, what happened today) and a 60 s timeout.
  - Only one job runs at a time, and jobs for the same user and point are deduplicated.
  - If the job fails or times out, the deterministic plan stands. The user gets a push with the error code (`E-COACH-CLAUDE-*`), so a missing decision is never silent.
- **Preferences:** a free-text note per user ("I like the park when it's dry and over 10 °C"), read by Claude, not by Go. Claude can update it when the user says so.

### Outputs

- **Push:** through the existing `sendToUser`, one per activation, nudge or decision. iOS notification actions can't run code without opening the app, so a notification tap opens `/~/now`.
- **Calendar mirror:** a read-only `plan` calendar per user. `schedule` rewrites its objects (one VEVENT per item, with uid = item id) in the same transaction, then bumps its ctag, so iOS Calendar shows the live plan next to the real appointments. CalDAV writes to it are refused.

### External data

- **Weather:** Open-Meteo (no key), cached for an hour, only fetched for decision prompts.
- **Travel:** learned trip times first. Before enough are known, a routing API (to be chosen) or a fixed default per place pair.

### Build order

1. `plan_items`, routines, `schedule`, `tick`, `/~/now`, the done/skip/more API, push on activation. Usable on its own, with routines set by hand.
2. The `/~/mcp` endpoint, so brain dumps and emails work from Claude Code and the Claude app.
3. Shortcuts signals: wake, leave and arrive, plus the Action Button.
4. Decision points with headless Claude, weather and travel.
5. Learned durations and travel times, chores with follow-ups, and the plan calendar mirror.

### Open questions

- **Running Claude on the server:** `claude -p` logged in with a subscription, or the API with a key? This decides cost and what happens at quota limits.
- **Auth for the Claude app connector:** custom connectors want OAuth, while the proxy does basic auth. Either a small OAuth shim or a per-user token on `/~/mcp`.
- **Routing provider** for travel times, or just learning them.
- **Work hours:** fixed (one appointment per weekday) or variable?
- **Pushiness:** how many nudges per item, and quiet hours.

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

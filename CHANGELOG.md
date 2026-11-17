# Changelog

## Unreleased

- Web UI at `/~/` (server-rendered, htmx 4): list, add and delete events and calendars; refreshes within 10 s when a CalDAV client changes the calendar. Errors carry `E-UI-…` codes.
- Storage moved from one `.ics` file per event to a local Turso database (`DATA_DIR/caldav.db`).
- Faster ctag polls (~15×) and calendar listings (~2×); event data is only loaded when requested.
- Writes are now durable (fsync on commit).
- Storage errors carry stable codes (`E-DB-…`) in both the log line and the HTTP response.

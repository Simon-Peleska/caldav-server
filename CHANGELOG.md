# Changelog

## Unreleased

- Web Push notifications for event alarms, per device, including iPhone/iPad home-screen apps. Each device shows when it last got a notification, whether it was shown, or the error (`E-PUSH-…`); test buttons send to one or all devices.
- `POST /~/api/notify` sends a notification (title, body, url) to all devices of the authenticated user and reports the per-device result.

- Web UI at `/~/` (server-rendered, htmx 4): list (grouped by day), add and delete the events of the default calendar; refreshes within 10 s when a CalDAV client changes the calendar. Errors carry `E-UI-…` codes.
- Storage moved from one `.ics` file per event to a local Turso database (`DATA_DIR/caldav.db`).
- Faster ctag polls (~15×) and calendar listings (~2×); event data is only loaded when requested.
- Writes are now durable (fsync on commit).
- Storage errors carry stable codes (`E-DB-…`) in both the log line and the HTTP response.

# Changelog

## Unreleased

- Storage moved from one `.ics` file per event to a local Turso database (`DATA_DIR/caldav.db`).
- Faster ctag polls (~15×) and calendar listings (~2×); event data is only loaded when requested.
- Writes are now durable (fsync on commit).
- Storage errors carry stable codes (`E-DB-…`) in both the log line and the HTTP response.

# Gotchas

- **Turso driver needs a writable cache dir.** `tursogo` embeds a ~20 MB `.so` and extracts it at first use to `$TURSO_GO_CACHE_DIR` or the user cache dir; if that fails it panics (at startup, in `openDB`). The nix build sets `TURSO_GO_CACHE_DIR=$TMPDIR` in `preCheck` because the sandbox's `$HOME` is not writable. Systemd units with `DynamicUser`/no home need it set too.
- **Prepared statements don't help with tursogo.** The driver re-prepares SQL (`turso_connection_prepare_single`) on every execution even for a `*sql.Stmt`, so `db.Prepare` buys nothing. A point query costs ~15 µs, mostly FFI and prepare. Cut the *number* of queries per request instead.
- **Every pooled connection is a separate Turso database handle** on the same file. They do share state in-process (verified: concurrent writers across connections, read-your-writes, `integrity_check` ok), but keep `SetMaxIdleConns` equal to `SetMaxOpenConns` so handles aren't reopened constantly.
- **`name IN (...)` doesn't use the full primary key.** Turso plans `WHERE user=? AND cal=? AND name IN (...)` as a seek on `(user, cal)` plus a scan of the calendar, so it is slower than separate point lookups. Check `EXPLAIN QUERY PLAN` before batching.
- **`flake.nix` `vendorHash`** must be updated whenever `go.sum` changes: set it to a fake hash, run `nix build`, copy the `got:` hash.
- Dev shell: `go build` with VCS stamping fails outside the repo (e.g. scratch dirs) — use `-buildvcs=false` there.

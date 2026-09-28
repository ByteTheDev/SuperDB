@everyone

**SuperDB — Bug-Fix & Performance Update (PR #5 merged)**

Engine:
- WHERE now supports !=, <>, <, <=, >, >= everywhere (was silently broken)
- '' escapes to ' in literals; apostrophes round-trip via params
- Unknown columns in WHERE/ORDER BY/aggregates now error
- ORDER BY spacing fix; non-PK tables work; multibyte CREATE INDEX fix

Client/Server:
- Client.Close sends TypeClose; dead connections fail fast
- Connection strings no longer double-decode %XX
- Local atomic batches are truly atomic
- Unset query timeout = unbounded; remote auth + MaxConnections fixed

Perf: JSON U+2028/29 parity, dead code removed, fewer cluster allocs.

Storage/WAL/wire unchanged — fully compatible. All tests + race pass.

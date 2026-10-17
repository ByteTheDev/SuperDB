@everyone

**SuperDB v0.6.0 — SQL Surface, Integrity & TLS**

SQL:
- New DDL/introspection: CREATE INDEX, ALTER ADD/DROP COLUMN, DROP TABLE/INDEX, TRUNCATE, SHOW TABLES/COLUMNS/INDEX, DESCRIBE
- GROUP BY + HAVING over COUNT/SUM/MIN/MAX/AVG
- SELECT DISTINCT, OFFSET, and WHERE IN / BETWEEN / LIKE
- INSERT OR REPLACE (in-place upsert) and INSERT INTO ... SELECT
- New interactive superdb-cli shell (.format, .timing, multi-line) plus --file/--format/--timing/--ping

Storage & backups:
- Snapshot/WAL frames now carry a CRC-32C checksum (v3); older files still readable
- Backups write a SHA-256 manifest; verify with `superdb verify-backup [FILE|DIR]`
- compact/recover now remove the WAL so folded records can't double-apply on restart

Server:
- Local-server TLS 1.2+: `server --tls-cert/--tls-key`; clients use `--tls`/`--tls-ca`/`--tls-skip-verify`
- Protocol `params` for `?` placeholders; hosted `/ready` readiness + `/metrics`

Fixed: typed-equality SELECT fast paths (id = 01, v = 1.50, k = TRUE), case-variant JSON keys,
unknown-column/aggregate validation, WAL writer after close.

Storage/protocol changes are backward compatible. All tests + race pass.

Download: https://github.com/ByteTheDev/SuperDB/releases/tag/v0.6.0

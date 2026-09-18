# SUPERDB Compressed Storage Design

## Goal

Reduce on-disk storage for SuperDB snapshots and write-ahead logs while keeping the client-facing TCP protocol unchanged. Clients continue to send SQL inside ordinary JSON requests and receive ordinary JSON results.

## Scope

- Compress snapshot data written to `superdb.snapshot`.
- Compress each WAL SQL statement as an independently framed append-only record in `superdb.wal`.
- Restore compressed snapshots and replay compressed WAL records on startup.
- Continue reading the current legacy formats:
  - snapshots containing plain JSON;
  - WAL files containing one SQL statement per line.
- Do not compress network requests or responses in this change.

## Format

The format uses only Go standard-library primitives and a versioned SUPERDB header.

### Snapshot

By default, the file is `SUPERDB/snapshot.spdb`. It contains a fixed header identifying a SuperDB snapshot and its format version, followed by a zlib stream containing the existing JSON representation of `engine.Database`.

Snapshot loading first recognizes the SUPERDB header. If it is absent, the loader attempts to parse the file as legacy JSON. Invalid headers, decompression errors, and invalid JSON are returned as startup errors.

### WAL

By default, the file is `SUPERDB/wal.spdb`. Each append is one framed record containing a SUPERDB WAL magic/version marker, the compressed and uncompressed payload lengths, and a zlib-compressed SQL statement. Records are independently decodable so appends do not require rewriting the whole file.

Replay detects the framed format and decodes every record. If the marker is absent, it falls back to the existing newline-delimited SQL format. Truncated or corrupt framed records fail replay with an error that identifies the WAL record.

## Data flow

1. A client sends a length-prefixed JSON request over TCP.
2. The server decodes JSON and executes SQL normally.
3. On a mutating request, WAL mode appends a compressed framed SQL record, or snapshot mode writes a compressed snapshot.
4. The server serializes the `Result` or error map as ordinary JSON and returns it over TCP.
5. On startup, the server restores a snapshot when present, then replays WAL when running in WAL mode.

## Safety and compatibility

- Snapshot writes go to a temporary file in the same directory, then replace the target file, preventing a partially written snapshot from being treated as valid.
- The file format is explicitly versioned so future SUPERDB formats can be rejected or added without guessing.
- Existing files and the previous `superdb.snapshot`/`superdb.wal` filenames remain readable; new writes use the compressed `.spdb` format.
- Network framing and JSON payloads remain unchanged, so the current CLI continues to work.

## Testing

Tests will cover:

- compression/decompression round trips;
- snapshot round trips and legacy JSON loading;
- WAL append/replay and legacy line-WAL replay;
- corrupt/truncated data errors;
- startup restoration from a snapshot;
- unchanged JSON behavior for server responses.

## Non-goals

- A custom binary representation of table rows.
- Compression negotiation or compression of network traffic.
- Changing SQL semantics or query result shapes.

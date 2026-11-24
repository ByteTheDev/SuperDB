# SuperDB wire protocol (v1)

RESP subset (Redis-compatible framing) plus inline telnet commands.
Implemented in `src/sdb_resp.*`, dispatched in `src/sdb_cmd.c`.

## Requests

Either form; the server detects `*` (RESP) vs a text line (inline).

RESP array of bulk strings:
```
*<n>\r\n ($<len>\r\n<bytes>\r\n) * n
```
Inline (for `nc`/`telnet`; values with spaces OK, binary needs RESP):
```
SET foo hello world\n      -> SET key "hello world"
GET foo\n
```

Keys: ≤ 64 KB, no embedded NUL. Values: arbitrary bytes (RESP only).

## Replies

| Prefix | Meaning | Example |
|---|---|---|
| `+` | status | `+OK\r\n`, `+PONG\r\n` |
| `-` | error | `-ERR unknown command\r\n` |
| `:` | integer | `:16\r\n` |
| `$` | bulk (`$-1` = nil) | `$5\r\nhello\r\n` |
| `*` | array (KEYS) | `*2\r\n$3\r\nfoo\r\n...` |

## Commands

PING [msg] · ECHO msg · SET key value [EX seconds] · GET key ·
DEL key [key...] (-> count) · EXISTS key [key...] (-> count) ·
KEYS [prefix*] (-> array) · EXPIRE key seconds (-> 1/0) ·
TTL key (seconds, -1 = no TTL, -2 = missing) ·
PTTL key (milliseconds) · INCR key [by] · INCRBY key delta ·
DBSIZE · INFO · COMPACT (aliases: SAVE, BGREWRITEAOF) ·
FLUSHALL · QUIT/EXIT (close) · SHUTDOWN (stop server) ·
COMMAND/HELP (command list)

## Notes

- Pipelining works: the server drains all complete requests per `recv`.
- `KEYS` returns a RESP array of bulk strings; empty db returns `*0`.
- Binary values round-trip exactly over RESP (length-prefixed).

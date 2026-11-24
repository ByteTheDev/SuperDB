# SuperDB wire protocol

RESP subset (Redis-compatible framing) plus inline telnet commands.
Implemented in `src/proto/superdb_resp.*`, dispatched in `src/proto/superdb_cmd.c`
and backed by the SDB2 log format.

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

Connection: PING [msg] · ECHO msg · AUTH password · QUIT/EXIT (close) ·
SHUTDOWN (stop server) · COMMAND/HELP (command list)

Strings: SET key value [EX seconds|PX ms] · SETNX key value · GET key ·
GETSET key value · MSETNX key value [key value...] ·
INCR key [by] · INCRBY key delta

Keys: DEL key [key...] (-> count) · EXISTS key [key...] (-> count) ·
KEYS [prefix*] (-> array) · SCAN cursor [MATCH pattern] [COUNT n] ·
TYPE key · RENAME src dst · EXPIRE key seconds · EXPIREAT key unix-seconds ·
PERSIST key · TTL key (seconds, -1 = no TTL, -2 = missing) ·
PTTL key (milliseconds) · DBSIZE · FLUSHALL

Lists: LPUSH/RPUSH key value [value...] (-> length) · LPOP/RPOP key ·
LLEN key · LRANGE key start stop

Hashes: HSET key field value [field value...] · HGET key field ·
HDEL key field [field...] · HLEN key · HGETALL key (interleaved field/value)

Sets: SADD/SREM key member [member...] (-> count) · SISMEMBER key member ·
SCARD key · SMEMBERS key

Server: INFO · SLOWLOG [GET|LEN|RESET] · SYNCLOG offset [maxlen] ·
MULTI/EXEC/DISCARD · COMPACT (aliases: SAVE, BGREWRITEAOF) ·
BACKUP dst-path

Typed commands return `-WRONGTYPE ...` when the key holds another type.

## Notes

- Pipelining works: the server drains all complete requests per `recv`.
- Connections are handled concurrently (one thread each, `--max-clients` limit).
- If `--requirepass`/`SUPERDB_PASSWORD` is set, all commands except AUTH need
  authentication; password comparison is constant-time.
- With `--replicaof HOST:PORT` the server is read-only (writes return `-READONLY`)
  and pulls the primary's log; SYNCLOG exposes it to replicas.
- MULTI queues commands per connection; EXEC replies with an array of results.
- `KEYS`/`SCAN` return RESP arrays of bulk strings; an empty db returns `*0`.
- Binary values round-trip exactly over RESP (length-prefixed).

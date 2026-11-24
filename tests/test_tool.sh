#!/bin/sh
# test_tool.sh — roundtrip test for the `superdb` project CLI.
# Offline only, except an optional start/status/stop probe on a high port.
# Usage: sh tests/test_tool.sh [--bindir DIR] [--srcdir DIR]
set -u

BINDIR="./bin"
SRCDIR="."
while [ $# -gt 0 ]; do
  case "$1" in
    --bindir) BINDIR="$2"; shift 2 ;;
    --bindir=*) BINDIR="${1#--bindir=}"; shift ;;
    --srcdir) SRCDIR="$2"; shift 2 ;;
    --srcdir=*) SRCDIR="${1#--srcdir=}"; shift ;;
    *) echo "test_tool: unknown flag $1" >&2; exit 2 ;;
  esac
done

fail() { echo "test_tool: FAIL: $1" >&2; exit 1; }
pass() { echo "test_tool: ok: $1"; }

T=$(mktemp -d 2>/dev/null || mktemp -d -t superdb-tool) || fail "mktemp"
trap 'rm -rf "$T"' EXIT INT TERM
# resolve to absolute paths: the test cd's into a temp dir below
case "$BINDIR" in
  /*|[A-Za-z]:/*) ;;
  *) BINDIR="$(pwd)/$BINDIR" ;;
esac
case "$SRCDIR" in
  /*|[A-Za-z]:/*) ;;
  *) SRCDIR="$(pwd)/$SRCDIR" ;;
esac
SRC="$SRCDIR"
SDB="$BINDIR/superdb"
[ -x "$SDB" ] || SDB="$SDB.exe" # Windows

[ -x "$SDB" ] || fail "binary not found: $SDB"
"$SDB" version >/dev/null 2>&1 || fail "version"

cd "$T" || fail "cd tmp"
"$SDB" init demo --db ./data.sdb --port 17381 >/dev/null || fail "init"
[ -f demo/superdb.json ] || fail "init: superdb.json missing"
[ -f demo/.vscode/settings.json ] || fail "init: .vscode/settings.json missing"
[ -f demo/.vscode/extensions.json ] || fail "init: .vscode/extensions.json missing"
[ -f demo/superdb_example.c ] || fail "init: example missing"
pass "init"

"$SDB" install demo --from "$SRC" >/dev/null || fail "install"
[ -f demo/third_party/superdb/include/superdb.h ] || fail "install: header missing"
[ -f demo/third_party/superdb/src/core/superdb.c ] || fail "install: sources missing"
[ -f demo/third_party/superdb/README.md ] || fail "install: README missing"
pass "install"

cd demo || fail "cd demo"
"$SDB" --db ./data.sdb set greeting hello >/dev/null || fail "set"
[ "$("$SDB" --db ./data.sdb get greeting)" = "hello" ] || fail "get"
"$SDB" --db ./data.sdb set n 41 >/dev/null || fail "set n"
[ "$("$SDB" --db ./data.sdb incr n)" = "(integer) 42" ] || fail "incr"
"$SDB" --db ./data.sdb keys '*' | grep -q greeting || fail "keys"
"$SDB" --db ./data.sdb expire greeting 60 >/dev/null || fail "expire"
[ "$("$SDB" --db ./data.sdb ttl greeting)" = "(integer) 60" ] || fail "ttl"
[ "$("$SDB" --db ./data.sdb dbsize)" = "(integer) 2" ] || fail "dbsize"
"$SDB" --db ./data.sdb info | grep -q '^keys: 2' || fail "info"
pass "offline kv"

"$SDB" --db ./data.sdb dump --out dump.json >/dev/null || fail "dump"
grep -q '"k":"greeting"' dump.json || fail "dump content"
grep -q '"ttl_ms":' dump.json || fail "dump ttl"
rm -f data.sdb
"$SDB" --db ./data.sdb restore --in dump.json >/dev/null || fail "restore"
[ "$("$SDB" --db ./data.sdb get greeting)" = "hello" ] || fail "restore content"
"$SDB" --db ./data.sdb inspect | grep -q 'keys: 2' || fail "inspect"
pass "dump/restore/inspect"

"$SDB" doctor >/dev/null || fail "doctor"
"$SDB" status >/dev/null 2>&1 && fail "status should fail when stopped"
pass "doctor/status"

# Optional lifecycle probe (skip if the port is already taken).
if "$SDB" start >/dev/null 2>&1; then
  sleep 1
  "$SDB" status >/dev/null || fail "status after start"
  "$SDB" stop >/dev/null || fail "stop"
  pass "start/stop"
else
  echo "test_tool: SKIP: start (port busy or no server binary)"
fi

echo "test_tool: ALL OK"

#!/bin/sh
# install.sh — build + install SuperDB (POSIX).
#   sh tools/install.sh [--prefix ~/.local] [--no-assoc] [--no-vscode]
# Installs: superdb-server, superdb-cli, superdb, libsuperdb.a, superdb.h,
# .sdb association (Linux), VS Code extension.
set -u

PREFIX="${HOME}/.local"
ASSOC=1
VSCODE=1
for a in "$@"; do
  case "$a" in
    --prefix=*) PREFIX="${a#--prefix=}" ;;
    --prefix) PREFIX="$2"; shift ;;
    --no-assoc) ASSOC=0 ;;
    --no-vscode) VSCODE=0 ;;
    -h|--help) echo "usage: sh tools/install.sh [--prefix DIR] [--no-assoc] [--no-vscode]"; exit 0 ;;
    *) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
  shift 2>/dev/null || true
done

cd "$(dirname "$0")/.." || exit 1
echo "==> building (make)"
make || exit 1

echo "==> installing to $PREFIX"
make install PREFIX="$PREFIX" || exit 1
case ":$PATH:" in
  *":$PREFIX/bin:"*) ;;
  *) echo "NOTE: add to PATH: export PATH=\"$PREFIX/bin:\$PATH\"" ;;
esac

if [ "$ASSOC" = 1 ] && [ "$(uname)" = "Linux" ]; then
  echo "==> associating *.sdb"
  "$PREFIX/bin/superdb" assoc || echo "(assoc skipped: $?)"
fi

if [ "$VSCODE" = 1 ]; then
  DEST="$HOME/.vscode/extensions/superdb.vscode-superdb-0.1.0"
  echo "==> installing VS Code extension to $DEST"
  mkdir -p "$DEST"
  cp -r editors/vscode-superdb/. "$DEST"/
  echo "reload VS Code to activate."
fi

echo "install OK — try: superdb doctor"

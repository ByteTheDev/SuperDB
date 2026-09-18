#!/usr/bin/env sh
set -eu

if [ "$#" -lt 2 ]; then
    echo "usage: $0 VERSION GOOS [GOARCH] [OUTPUT_DIR]" >&2
    exit 2
fi

version="$1"
goos="$2"
goarch="${3:-$(go env GOARCH)}"
output_dir="${4:-dist}"
version="${version#v}"
root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
package_name="superdb_${version}_${goos}_${goarch}"
stage_dir="$root_dir/$output_dir/$package_name"

rm -rf "$stage_dir"
mkdir -p "$stage_dir"

extension=""
if [ "$goos" = "windows" ]; then
    extension=".exe"
fi

(
    cd "$root_dir"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage_dir/superdb$extension" ./cmd/superdb
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage_dir/superdb-cli$extension" ./cmd/superdb-cli
)
cp "$root_dir/README.md" "$stage_dir/README.md"

if [ "$goos" = "windows" ]; then
    (cd "$root_dir/$output_dir" && zip -q -r "$package_name.zip" "$package_name")
else
    tar -C "$root_dir/$output_dir" -czf "$root_dir/$output_dir/$package_name.tar.gz" "$package_name"
fi
rm -rf "$stage_dir"

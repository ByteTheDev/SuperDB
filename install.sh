#!/usr/bin/env sh
set -eu

repository="${SUPERDB_REPOSITORY:-ByteTheDev/SuperDB}"
version="${SUPERDB_VERSION:-}"
install_dir="${SUPERDB_INSTALL_DIR:-/usr/local/bin}"

usage() {
    echo "Usage: install.sh [--version VERSION] [--install-dir DIR] [--no-path]"
}

add_path=1
while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            version="$2"
            shift 2
            ;;
        --install-dir)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            install_dir="$2"
            shift 2
            ;;
        --no-path)
            add_path=0
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            usage >&2
            exit 2
            ;;
    esac
done

command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { echo "tar is required" >&2; exit 1; }

case "$(uname -s)" in
    Linux) os="linux" ;;
    Darwin) os="darwin" ;;
    *) echo "Unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [ -z "$version" ]; then
    version=$(curl -fsSL "https://api.github.com/repos/$repository/releases/latest" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
fi
[ -n "$version" ] || { echo "Could not determine the latest SuperDB version" >&2; exit 1; }
version=${version#v}
archive="superdb_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/$repository/releases/download/v$version"

tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t superdb)
trap 'rm -rf "$tmp_dir"' EXIT INT TERM
curl -fsSL "$base_url/$archive" -o "$tmp_dir/$archive"
curl -fsSL "$base_url/SHA256SUMS" -o "$tmp_dir/SHA256SUMS"
expected=$(awk -v file="$archive" '$2 == file { print $1; exit }' "$tmp_dir/SHA256SUMS")
[ -n "$expected" ] || { echo "Checksum missing for $archive" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$tmp_dir/$archive" | awk '{print $1}')
else
    actual=$(shasum -a 256 "$tmp_dir/$archive" | awk '{print $1}')
fi
[ "$expected" = "$actual" ] || { echo "Checksum verification failed" >&2; exit 1; }

tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
package_dir="$tmp_dir/superdb_${version}_${os}_${arch}"

if [ ! -d "$install_dir" ] || [ ! -w "$install_dir" ]; then
    if [ "$install_dir" = "/usr/local/bin" ] && command -v sudo >/dev/null 2>&1; then
        sudo mkdir -p "$install_dir"
        sudo install -m 0755 "$package_dir/superdb" "$install_dir/superdb"
        sudo install -m 0755 "$package_dir/superdb-cli" "$install_dir/superdb-cli"
    else
        install_dir="${HOME}/.local/bin"
        mkdir -p "$install_dir"
        install "$package_dir/superdb" "$install_dir/superdb"
        install "$package_dir/superdb-cli" "$install_dir/superdb-cli"
    fi
else
    mkdir -p "$install_dir"
    install -m 0755 "$package_dir/superdb" "$install_dir/superdb"
    install -m 0755 "$package_dir/superdb-cli" "$install_dir/superdb-cli"
fi

echo "Installed SuperDB $version to $install_dir"
if [ "$add_path" -eq 1 ] && [ "$install_dir" = "${HOME}/.local/bin" ]; then
    case ":${PATH}:" in
        *":$install_dir:"*) ;;
        *) echo "Add $install_dir to PATH to use SuperDB from every shell." ;;
    esac
fi

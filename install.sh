#!/bin/sh
# Installs lal, the louisiane command, from the latest GitHub release:
#
#     curl -fsSL https://raw.githubusercontent.com/jrimmer/louisiane/main/install.sh | sh
#
# LAL_VERSION=v0.1.0 picks a release; LAL_INSTALL_DIR picks where it goes
# (default ~/.local/bin, or /usr/local/bin as root); LAL_BASE_URL fetches
# from somewhere else (a mirror, or a local build for testing). Linux on
# x86-64 or ARM64.
set -eu

repo=jrimmer/louisiane
version=${LAL_VERSION:-latest}

case "$(uname -s)" in
Linux) os=linux ;;
*) echo "louisiane runs on Linux for now (this is $(uname -s))." >&2; exit 1 ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "no louisiane build for $(uname -m)" >&2; exit 1 ;;
esac

if [ "$(id -u)" = 0 ]; then dir=${LAL_INSTALL_DIR:-/usr/local/bin}; else dir=${LAL_INSTALL_DIR:-$HOME/.local/bin}; fi
if [ -n "${LAL_BASE_URL:-}" ]; then
	base=$LAL_BASE_URL
elif [ "$version" = latest ]; then
	base="https://github.com/$repo/releases/latest/download"
else
	base="https://github.com/$repo/releases/download/$version"
fi
asset="lal_${os}_${arch}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fetch() {
	if command -v curl >/dev/null; then curl -fsSL -o "$2" "$1"; else wget -qO "$2" "$1"; fi
}
fetch "$base/$asset" "$tmp/$asset"
fetch "$base/checksums.txt" "$tmp/checksums.txt"
(cd "$tmp" && grep " $asset\$" checksums.txt | sha256sum -c - >/dev/null) || {
	echo "checksum mismatch for $asset; not installing" >&2
	exit 1
}
mkdir -p "$dir"
install -m 0755 "$tmp/$asset" "$dir/lal"
echo "Installed $("$dir/lal" version) to $dir/lal"
case ":$PATH:" in *":$dir:"*) ;; *) echo "Add $dir to your PATH." ;; esac
cat <<'NEXT'

Next:
  on an always-on machine:   sudo lal relay init
  on each machine with agents:  lal join TOKEN   (from `lal invite` on the relay's machine)
  in each project:           lal init
NEXT

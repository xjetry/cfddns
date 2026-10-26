#!/bin/sh
# cfddns installer: picks the release asset for this machine, downloads it and
# runs "cfddns -install" with every argument you pass. Works with busybox sh
# (OpenWrt, Alpine) and bash. Must run as root.
#
#   wget -qO- https://raw.githubusercontent.com/xjetry/cfddns/main/install.sh \
#     | sh -s -- -4 home.example.com -token "$CF_TOKEN"
#
# Environment:
#   CFDDNS_VERSION  release tag to install (default: latest)
#   CFDDNS_ASSET    force an asset suffix (amd64, arm64, armv7, armv6, mips, mipsle)
set -eu

REPO="${CFDDNS_REPO:-xjetry/cfddns}"
VERSION="${CFDDNS_VERSION:-latest}"

die() { echo "install.sh: $*" >&2; exit 1; }

detect_asset() {
	case "$(uname -m)" in
		x86_64|amd64) echo amd64 ;;
		aarch64|arm64) echo arm64 ;;
		armv7*|armv8l) echo armv7 ;;
		armv6*) echo armv6 ;;
		mips|mipsel|mipsle)
			# busybox reports "mips" for both byte orders; read EI_DATA
			# (byte 5 of the ELF header, 1 = little endian) from the shell.
			b="$(dd if=/bin/sh bs=1 skip=5 count=1 2>/dev/null)"
			if [ "$b" = "$(printf '\001')" ]; then echo mipsle; else echo mips; fi
			;;
		*) die "unsupported architecture $(uname -m); set CFDDNS_ASSET to force one" ;;
	esac
}

fetch() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		die "need curl or wget"
	fi
}

[ "$(id -u)" = 0 ] || die "run as root (sudo)"
[ $# -gt 0 ] || die "pass the cfddns -install flags, e.g.: -4 home.example.com -token XXX"

asset="${CFDDNS_ASSET:-$(detect_asset)}"
if [ "$VERSION" = latest ]; then
	url="https://github.com/$REPO/releases/latest/download/cfddns-linux-$asset"
else
	url="https://github.com/$REPO/releases/download/$VERSION/cfddns-linux-$asset"
fi

tmp="$(mktemp 2>/dev/null || echo "/tmp/cfddns.$$")"
trap 'rm -f "$tmp"' EXIT

echo "downloading $url"
fetch "$url" "$tmp"
chmod +x "$tmp"
"$tmp" -install "$@"

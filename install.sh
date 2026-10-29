#!/bin/sh
# cfddns installer: picks the release asset for this machine, downloads it,
# checks it against the release's SHA256SUMS and runs "cfddns -install" with
# every argument you pass. Works with busybox sh (OpenWrt, Alpine) and bash.
# Must run as root.
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

# The download lands in a directory that holds system binaries, never in
# /tmp: /tmp is often mounted noexec on hardened hosts, where running the
# binary from it fails after the download, and it is world-writable, where a
# predictable name could be pre-placed as a symlink by any local user. These
# directories are executable by definition and writable by root only.
workdir() {
	for d in /usr/local/bin /usr/bin; do
		if [ -d "$d" ] && [ -w "$d" ]; then
			echo "$d"
			return
		fi
	done
	die "no writable binary directory (/usr/local/bin or /usr/bin)"
}

[ "$(id -u)" = 0 ] || die "run as root (sudo)"
[ $# -gt 0 ] || die "pass the cfddns -install flags, e.g.: -4 home.example.com -token XXX"

if command -v sha256sum >/dev/null 2>&1; then
	sha256="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
	sha256="shasum -a 256"
else
	die "need sha256sum or shasum to verify the download"
fi

asset="${CFDDNS_ASSET:-$(detect_asset)}"
if [ "$VERSION" = latest ]; then
	base="https://github.com/$REPO/releases/latest/download"
else
	base="https://github.com/$REPO/releases/download/$VERSION"
fi

dir="$(workdir)"
tmp="$(mktemp "$dir/.cfddns-install.XXXXXX" 2>/dev/null || echo "$dir/.cfddns-install.$$")"
sums="$tmp.sha256"
trap 'rm -f "$tmp" "$sums"' EXIT

echo "downloading $base/cfddns-linux-$asset"
fetch "$base/cfddns-linux-$asset" "$tmp"
fetch "$base/SHA256SUMS" "$sums"
# SHA256SUMS lines read "<hash>  cfddns-linux-<asset>"; anchor the name so
# "mips" cannot match "mipsle".
want="$(grep "[[:space:]]cfddns-linux-$asset\$" "$sums" | cut -d' ' -f1 || true)"
[ -n "$want" ] || die "cfddns-linux-$asset is not listed in SHA256SUMS"
got="$($sha256 "$tmp" | cut -d' ' -f1)"
[ "$got" = "$want" ] || die "checksum mismatch for cfddns-linux-$asset: got $got, want $want"
chmod +x "$tmp"
"$tmp" -install "$@"

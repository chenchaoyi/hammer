#!/bin/sh
set -eu

repo="${HAMMER_REPO:-chenchaoyi/hammer}"
version="${HAMMER_VERSION:-}"
install_dir="${HAMMER_INSTALL_DIR:-}"

need() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "hammer installer: missing required command: $1" >&2
		exit 1
	fi
}

default_install_dir() {
	if [ -n "${PREFIX:-}" ]; then
		printf '%s/bin\n' "$PREFIX"
		return
	fi
	if [ "$(id -u 2>/dev/null || printf 1)" = "0" ]; then
		printf '%s\n' "/usr/local/bin"
		return
	fi
	if [ -d "/usr/local/bin" ] && [ -w "/usr/local/bin" ]; then
		printf '%s\n' "/usr/local/bin"
		return
	fi
	printf '%s\n' "${HOME:-.}/.local/bin"
}

detect_os() {
	raw="${HAMMER_OS:-$(uname -s)}"
	case "$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]')" in
		darwin | macos) printf '%s\n' "darwin" ;;
		linux) printf '%s\n' "linux" ;;
		*)
			echo "hammer installer: unsupported OS: $raw" >&2
			exit 1
			;;
	esac
}

detect_arch() {
	raw="${HAMMER_ARCH:-$(uname -m)}"
	case "$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]')" in
		x86_64 | amd64) printf '%s\n' "amd64" ;;
		arm64 | aarch64) printf '%s\n' "arm64" ;;
		*)
			echo "hammer installer: unsupported architecture: $raw" >&2
			exit 1
			;;
	esac
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		echo "hammer installer: missing sha256sum or shasum" >&2
		exit 1
	fi
}

verify_checksum() {
	archive="$1"
	asset="$2"
	sums="$3"
	expected="$(awk -v file="$asset" '$2 == file {print $1}' "$sums")"
	if [ -z "$expected" ]; then
		echo "hammer installer: $asset not found in SHA256SUMS" >&2
		exit 1
	fi
	actual="$(sha256_of "$archive")"
	if [ "$actual" != "$expected" ]; then
		echo "hammer installer: checksum mismatch for $asset" >&2
		echo "expected: $expected" >&2
		echo "actual:   $actual" >&2
		exit 1
	fi
}

validate_download() {
	file="$1"
	kind="$2"
	[ -s "$file" ] || return 1
	case "$kind" in
		tarball)
			gzip -t "$file" >/dev/null 2>&1
			;;
		sums)
			awk -v file="$asset" '$2 == file && $1 ~ /^[0-9a-fA-F]{64}$/ { ok=1 } END { exit ok ? 0 : 1 }' "$file"
			;;
		*)
			return 0
			;;
	esac
}

mirror_host() {
	host="${1#http://}"
	host="${host#https://}"
	printf '%s\n' "${host%%/*}"
}

build_candidates() {
	url="$1"
	force_github_first="$2"

	case "$url" in
		https://github.com/*) ;;
		*)
			printf 'direct %s\n' "$url"
			return
			;;
	esac

	if [ "$mirror_mode" = "github" ] || [ "$mirror_mode" = "auto" ] || [ "$mirror_mode" = "custom" ] || { [ "$force_github_first" = "1" ] && [ "$mirror_mode" != "ghproxy" ]; }; then
		printf 'github %s\n' "$url"
	fi
	if [ "$mirror_mode" != "github" ]; then
		printf '%s\n' "$mirror_proxies" | while IFS= read -r proxy; do
			[ -z "$proxy" ] && continue
			printf '%s %s%s\n' "${proxy%/}" "$proxy" "$url"
		done
	fi
}

download_with_fallback() {
	url="$1"
	dest="$2"
	kind="$3"
	force_github_first="${4:-0}"
	DOWNLOAD_USED_SRC=""

	while IFS=' ' read -r src candidate_url; do
		[ -z "$candidate_url" ] && continue
		if [ "$src" != "github" ] && [ "$src" != "direct" ]; then
			echo "GitHub download failed or returned invalid content; retrying via $(mirror_host "$src") mirror..."
		fi
		if curl --max-time 20 -fsSL -o "$dest" "$candidate_url" 2>/dev/null && validate_download "$dest" "$kind"; then
			DOWNLOAD_USED_SRC="$src"
			return 0
		fi
		rm -f "$dest"
	done <<EOF
$(build_candidates "$url" "$force_github_first")
EOF

	return 1
}

need curl
need tar
need awk
need gzip

if [ -z "$install_dir" ]; then
	install_dir="$(default_install_dir)"
fi

os="$(detect_os)"
arch="$(detect_arch)"
asset="hammer-${os}-${arch}.tar.gz"

mirror_proxy_chain='https://ghfast.top/
https://gh-proxy.com/
https://ghproxy.net/'

case "${HAMMER_INSTALL_MIRROR:-auto}" in
	auto)
		mirror_mode="auto"
		mirror_proxies="$mirror_proxy_chain"
		;;
	github)
		mirror_mode="github"
		mirror_proxies=""
		;;
	ghproxy)
		mirror_mode="ghproxy"
		mirror_proxies="$mirror_proxy_chain"
		;;
	http://* | https://*)
		mirror_mode="custom"
		mirror_proxies="${HAMMER_INSTALL_MIRROR%/}/"
		;;
	*)
		echo "hammer installer: unknown HAMMER_INSTALL_MIRROR value: ${HAMMER_INSTALL_MIRROR} (expected auto|github|ghproxy|<http(s)://proxy/>)" >&2
		exit 1
		;;
esac

if [ -n "${HAMMER_DOWNLOAD_BASE:-}" ]; then
	base="${HAMMER_DOWNLOAD_BASE%/}"
elif [ -n "$version" ]; then
	base="https://github.com/${repo}/releases/download/${version}"
else
	base="https://github.com/${repo}/releases/latest/download"
fi

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t hammer)"
trap 'rm -rf "$tmp"' EXIT INT TERM

archive="$tmp/$asset"
sums="$tmp/SHA256SUMS"
extract_dir="$tmp/extract"
mkdir -p "$extract_dir"

echo "Downloading $asset"
if ! download_with_fallback "$base/$asset" "$archive" tarball 0; then
	echo "hammer installer: GitHub and every configured mirror failed for $base/$asset" >&2
	exit 1
fi
archive_src="$DOWNLOAD_USED_SRC"

if [ "${HAMMER_SKIP_CHECKSUM:-}" != "1" ]; then
	if ! download_with_fallback "$base/SHA256SUMS" "$sums" sums 1; then
		echo "hammer installer: GitHub and every configured mirror failed for $base/SHA256SUMS" >&2
		exit 1
	fi
	sums_src="$DOWNLOAD_USED_SRC"
	verify_checksum "$archive" "$asset" "$sums"
	if [ "$archive_src" != "github" ] && [ "$archive_src" != "direct" ]; then
		echo "tarball fetched via $(mirror_host "$archive_src") mirror; SHA256 verified"
	elif [ "$sums_src" != "github" ] && [ "$sums_src" != "direct" ]; then
		echo "SHA256SUMS fetched via $(mirror_host "$sums_src") mirror; SHA256 verified"
	fi
fi

tar -xzf "$archive" -C "$extract_dir"
bin="$extract_dir/hammer"
if [ ! -f "$bin" ]; then
	bin="$extract_dir/hammer-${os}-${arch}"
fi
if [ ! -f "$bin" ]; then
	echo "hammer installer: archive did not contain a hammer binary" >&2
	exit 1
fi

mkdir -p "$install_dir"
cp "$bin" "$install_dir/hammer"
chmod 0755 "$install_dir/hammer"

echo "hammer installed to $install_dir/hammer"
case ":$PATH:" in
	*":$install_dir:"*) ;;
	*) echo "Add $install_dir to PATH to run: hammer" ;;
esac

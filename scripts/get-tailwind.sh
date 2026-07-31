#!/bin/sh
# Download the pinned Tailwind standalone CLI.
#
# The standalone CLI is used instead of npm so that neither the production
# image nor a contributor's machine needs a Node runtime. Because this pulls a
# third-party binary into the build path, the version is pinned by the caller
# and the download is verified against the checksum published in the release's
# sha256sums.txt before the binary is used.
set -eu

VERSION="${1:?usage: get-tailwind.sh <version> <bindir>}"
BINDIR="${2:?usage: get-tailwind.sh <version> <bindir>}"

case "$(uname -s)" in
	Linux*)   os=linux ;;
	Darwin*)  os=macos ;;
	MINGW*|MSYS*|CYGWIN*) os=windows ;;
	*) echo "get-tailwind: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
	x86_64|amd64) arch=x64 ;;
	arm64|aarch64) arch=arm64 ;;
	*) echo "get-tailwind: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

asset="tailwindcss-${os}-${arch}"
out="${BINDIR}/tailwindcss"
if [ "$os" = "windows" ]; then
	asset="${asset}.exe"
	out="${out}.exe"
fi

if [ -x "$out" ] && "$out" --help >/dev/null 2>&1; then
	exit 0
fi

mkdir -p "$BINDIR"
base="https://github.com/tailwindlabs/tailwindcss/releases/download/${VERSION}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "get-tailwind: downloading ${asset} ${VERSION}"
curl -fsSL --retry 3 -o "${tmp}/${asset}" "${base}/${asset}"
curl -fsSL --retry 3 -o "${tmp}/sha256sums.txt" "${base}/sha256sums.txt"

# The published sums file lists every asset, as "<hash>  ./<asset>". Match on a
# leading slash or space rather than assuming either form.
expected="$(grep -E "[ /]${asset}\$" "${tmp}/sha256sums.txt" | awk '{print $1}')"
if [ -z "$expected" ]; then
	echo "get-tailwind: ${asset} not listed in sha256sums.txt for ${VERSION}" >&2
	cat "${tmp}/sha256sums.txt" >&2
	exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "${tmp}/${asset}" | awk '{print $1}')"
else
	actual="$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')"
fi

if [ "$expected" != "$actual" ]; then
	echo "get-tailwind: checksum mismatch for ${asset}" >&2
	echo "  expected ${expected}" >&2
	echo "  actual   ${actual}" >&2
	exit 1
fi

chmod +x "${tmp}/${asset}"
mv "${tmp}/${asset}" "$out"
echo "get-tailwind: installed ${out}"

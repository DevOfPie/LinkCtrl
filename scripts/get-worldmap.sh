#!/bin/sh
# Verify — and if necessary re-download — the vendored world-map TopoJSON.
#
# This is the product's first vendored *data* file, and it follows the htmx
# contract rather than inventing a third one: a pinned version, a SHA-256, a copy
# committed to the repository, and a gate that runs this script with
# VERIFY_ONLY=1 so a mismatch is fatal instead of silently repaired. A check that
# cannot fail is not a check.
#
# What it fetches is **world-atlas**, Mike Bostock's pre-built TopoJSON of
# **Natural Earth**. Natural Earth is explicitly public domain and asks for no
# attribution; world-atlas's packaging is ISC. That combination is why this file
# can be committed here at all, and it is recorded as D63.
#
# Note what is *not* shipped. This JSON is a build-time input: it is read by
# `internal/ui/geo/mapgen`, which converts it to Go source once, and the binary
# embeds only the generated SVG path data. Nothing at request time parses
# TopoJSON, `ui` stays stdlib-only, and the CSP is untouched.
set -eu

VERSION="${1:?usage: get-worldmap.sh <version> <sha256> <destination>}"
EXPECTED="${2:?usage: get-worldmap.sh <version> <sha256> <destination>}"
DEST="${3:?usage: get-worldmap.sh <version> <sha256> <destination>}"

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

if [ -f "$DEST" ]; then
	actual="$(sha256_of "$DEST")"
	if [ "$actual" = "$EXPECTED" ]; then
		echo "get-worldmap: ${DEST} matches world-atlas ${VERSION}"
		exit 0
	fi
	if [ -n "${VERIFY_ONLY:-}" ]; then
		echo "get-worldmap: ${DEST} does not match world-atlas ${VERSION}" >&2
		echo "  expected ${EXPECTED}" >&2
		echo "  actual   ${actual}" >&2
		echo "The committed blob differs from the pinned upstream release. Run" >&2
		echo "\`make worldmap\` to restore it, review why it changed, and re-run" >&2
		echo "\`make mapgen\` if it legitimately moved." >&2
		exit 1
	fi
	echo "get-worldmap: ${DEST} does not match world-atlas ${VERSION}; re-fetching" >&2
	echo "  expected ${EXPECTED}" >&2
	echo "  actual   ${actual}" >&2
fi

if [ -n "${VERIFY_ONLY:-}" ]; then
	echo "get-worldmap: ${DEST} is missing; it is committed and must be present" >&2
	exit 1
fi

mkdir -p "$(dirname "$DEST")"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

url="https://cdn.jsdelivr.net/npm/world-atlas@${VERSION}/countries-110m.json"
echo "get-worldmap: downloading world-atlas ${VERSION}"
curl -fsSL --retry 3 -o "${tmp}/countries-110m.json" "$url"

actual="$(sha256_of "${tmp}/countries-110m.json")"
if [ "$EXPECTED" != "$actual" ]; then
	echo "get-worldmap: checksum mismatch for world-atlas ${VERSION}" >&2
	echo "  expected ${EXPECTED}" >&2
	echo "  actual   ${actual}" >&2
	echo "npm package versions are immutable, so a mismatch means the pinned" >&2
	echo "checksum in the Makefile is wrong or the download was tampered with." >&2
	exit 1
fi

mv "${tmp}/countries-110m.json" "$DEST"
echo "get-worldmap: installed world-atlas ${VERSION} at ${DEST}"

#!/bin/sh
# Verify — and if necessary re-download — the vendored Swagger UI assets.
#
# Same treatment as htmx (scripts/get-htmx.sh): a fixed upstream artifact is
# committed so a fresh clone builds offline, and this script is what keeps the
# committed blobs verifiable rather than trusted — each file is checked against
# a checksum pinned in the Makefile. It runs offline when the copies match and
# only reaches the network when one does not.
#
# Only two files are vendored. swagger-ui-bundle.js and swagger-ui.css are all
# that SwaggerUIBundle needs; the standalone preset just adds the top bar with
# a URL box, which an embedded viewer pointing at its own spec has no use for.
#
# VERIFY_ONLY=1 turns the repair off and makes a mismatch fatal; see the same
# note in get-htmx.sh for why a gate must never repair.
set -eu

VERSION="${1:?usage: get-swagger.sh <version> <css-sha256> <js-sha256> <destdir>}"
CSS_SHA="${2:?usage: get-swagger.sh <version> <css-sha256> <js-sha256> <destdir>}"
JS_SHA="${3:?usage: get-swagger.sh <version> <css-sha256> <js-sha256> <destdir>}"
DESTDIR="${4:?usage: get-swagger.sh <version> <css-sha256> <js-sha256> <destdir>}"

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

fetch() { # file expected-sha
	file="$1"
	expected="$2"
	dest="${DESTDIR}/${file}"

	if [ -f "$dest" ] && [ "$(sha256_of "$dest")" = "$expected" ]; then
		echo "get-swagger: ${dest} matches swagger-ui ${VERSION}"
		return 0
	fi

	if [ -n "${VERIFY_ONLY:-}" ]; then
		echo "get-swagger: ${dest} does not match swagger-ui ${VERSION}" >&2
		echo "  expected ${expected}" >&2
		if [ -f "$dest" ]; then
			echo "  actual   $(sha256_of "$dest")" >&2
		else
			echo "  actual   (file missing)" >&2
		fi
		echo "The committed blob differs from the pinned upstream release. Run" >&2
		echo "\`make swagger-ui\` to restore it, and review why it changed." >&2
		exit 1
	fi

	mkdir -p "$DESTDIR"
	tmp="$(mktemp)"
	url="https://cdn.jsdelivr.net/npm/swagger-ui-dist@${VERSION#v}/${file}"
	echo "get-swagger: downloading ${file} ${VERSION}"
	curl -fsSL --retry 3 -o "$tmp" "$url"

	actual="$(sha256_of "$tmp")"
	if [ "$expected" != "$actual" ]; then
		rm -f "$tmp"
		echo "get-swagger: checksum mismatch for ${file}" >&2
		echo "  expected ${expected}" >&2
		echo "  actual   ${actual}" >&2
		echo "npm artifacts are immutable per version, so a mismatch means the" >&2
		echo "pinned checksum in the Makefile is wrong or the download was tampered with." >&2
		exit 1
	fi
	mv "$tmp" "$dest"
	echo "get-swagger: installed ${dest}"
}

fetch swagger-ui.css "$CSS_SHA"
fetch swagger-ui-bundle.js "$JS_SHA"

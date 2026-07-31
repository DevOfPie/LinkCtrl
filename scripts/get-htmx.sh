#!/bin/sh
# Verify — and if necessary re-download — the vendored htmx build.
#
# htmx is committed to the repository rather than fetched during the build, and
# the distinction from Tailwind is deliberate. app.css is *generated from this
# repository's own templates*, so it has to be built and cannot be committed
# without going stale. htmx is a fixed upstream artifact: committing it keeps
# `go build` and `go run` working on a fresh clone with no network, which is
# what "single binary, no Node" is supposed to mean.
#
# The cost of vendoring is a file nobody reads. That is what this script is for:
# it checks the committed copy against the checksum of the pinned upstream
# release, so the blob is verifiable rather than trusted. It runs offline when
# the copy already matches, and only reaches the network when it does not.
#
# VERIFY_ONLY=1 turns the repair off and makes a mismatch fatal. CI must set it:
# repairing is the right behaviour for a developer whose copy went stale, and
# exactly the wrong behaviour for a gate, which would otherwise overwrite a
# tampered blob and report success — a check that cannot fail is not a check.
set -eu

VERSION="${1:?usage: get-htmx.sh <version> <sha256> <destination>}"
EXPECTED="${2:?usage: get-htmx.sh <version> <sha256> <destination>}"
DEST="${3:?usage: get-htmx.sh <version> <sha256> <destination>}"

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
		echo "get-htmx: ${DEST} matches htmx ${VERSION}"
		exit 0
	fi
	if [ -n "${VERIFY_ONLY:-}" ]; then
		echo "get-htmx: ${DEST} does not match htmx ${VERSION}" >&2
		echo "  expected ${EXPECTED}" >&2
		echo "  actual   ${actual}" >&2
		echo "The committed blob differs from the pinned upstream release. Run" >&2
		echo "\`make htmx\` to restore it, and review why it changed." >&2
		exit 1
	fi
	echo "get-htmx: ${DEST} does not match htmx ${VERSION}; re-fetching" >&2
	echo "  expected ${EXPECTED}" >&2
	echo "  actual   ${actual}" >&2
fi

if [ -n "${VERIFY_ONLY:-}" ]; then
	echo "get-htmx: ${DEST} is missing; it is committed and must be present" >&2
	exit 1
fi

mkdir -p "$(dirname "$DEST")"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

url="https://github.com/bigskysoftware/htmx/releases/download/${VERSION}/htmx.min.js"
echo "get-htmx: downloading htmx ${VERSION}"
curl -fsSL --retry 3 -o "${tmp}/htmx.min.js" "$url"

actual="$(sha256_of "${tmp}/htmx.min.js")"
if [ "$EXPECTED" != "$actual" ]; then
	echo "get-htmx: checksum mismatch for htmx ${VERSION}" >&2
	echo "  expected ${EXPECTED}" >&2
	echo "  actual   ${actual}" >&2
	echo "Upstream release assets are immutable, so a mismatch means the pinned" >&2
	echo "checksum in the Makefile is wrong or the download was tampered with." >&2
	exit 1
fi

mv "${tmp}/htmx.min.js" "$DEST"
echo "get-htmx: installed htmx ${VERSION} at ${DEST}"

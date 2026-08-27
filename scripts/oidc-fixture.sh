#!/usr/bin/env bash
#
# The OIDC add-on, as the integration suite's fixture (M69).
#
# M69's acceptance test needs the real add-on — `DevOfPie/LinkCtrl-OIDC`, which is
# a different repository and not this one's to build from a checkout. It is
# fetched from the module proxy at a pinned version, built the way its own
# Makefile builds it, and the result is checked against the digest its published
# manifest names.
#
# **Why it is rebuilt rather than committed.** .gitignore already refuses a
# checked-in wasm module, for m60.md's reason: a multi-megabyte binary is a build
# input nobody reviews. The same argument applies here and applies harder, because
# the thing being installed is the module this product's whole add-on foundation
# was built toward.
#
# **Why it is built as its own main module.** `go build github.com/DevOfPie/LinkCtrl-OIDC`
# from some other module produces a *different* binary: `-trimpath` rewrites paths
# to `<module>@<version>/…` for a dependency and to `<module>/…` for the main
# module, and those bytes are in the binary. Copying the module out of the cache
# and building it there reproduces the published artifact exactly — verified, and
# that is what MODULE_SHA256 below asserts on every run.
#
# **What the pin buys.** A Go pseudo-version is immutable and publicly resolvable,
# and the module sum below is the proxy's own checksum for it. Together they are
# the whole of "the add-on this test installs is the add-on that was published",
# and a drift in either fails here rather than in a test whose subject is OIDC.
#
# Usage: scripts/oidc-fixture.sh [output-dir]
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

# The add-on, pinned. Bumping this means bumping all three lines together.
MODULE=github.com/DevOfPie/LinkCtrl-OIDC
VERSION=v0.0.0-20260827052551-90d4bd1e1ecf
# sha256 of the module the release published, which is also what its addon.json
# names. Reproduced by the build below; a mismatch is a refusal.
MODULE_SHA256=695385a1a796a689ad2b3b4f910f93c102adfe304728c5081ed215837cb4477e

OUT="${1:-test/integration/testdata/oidc}"

if [ -s "$OUT/oidc.wasm" ] && [ -s "$OUT/addon.json" ] && [ -s "$OUT/go.mod" ] &&
	[ -s "$OUT/LICENSE" ]; then
	have=$(sha256sum "$OUT/oidc.wasm" | cut -d' ' -f1)
	if [ "$have" = "$MODULE_SHA256" ]; then
		exit 0
	fi
fi

echo "oidc-fixture: fetching $MODULE@$VERSION"
info=$(GO111MODULE=on go mod download -json "$MODULE@$VERSION")
dir=$(printf '%s' "$info" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",\{0,1\}$/\1/p' | tr -d '"')
if [ -z "$dir" ] || [ ! -d "$dir" ]; then
	echo "oidc-fixture: the module proxy did not hand back a source directory" >&2
	printf '%s\n' "$info" >&2
	exit 1
fi

# The module cache is read-only, and a build writes nothing into the source
# directory but `go` insists on being able to. Copied to a scratch directory that
# is removed on the way out, success or failure.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -r "$dir/." "$work/"
chmod -R u+w "$work"

# The add-on's own build flags, and they are not decoration. `-buildvcs=false`
# stops Go stamping a VCS revision that would differ between a checkout and a
# module cache; `-trimpath` is what makes the bytes a function of the source
# alone; `-buildmode=c-shared` is the reactor shape this host instantiates.
echo "oidc-fixture: building the module for wasip1"
(cd "$work" && GOOS=wasip1 GOARCH=wasm \
	go build -trimpath -buildvcs=false -buildmode=c-shared -o oidc.wasm .)

built=$(sha256sum "$work/oidc.wasm" | cut -d' ' -f1)
if [ "$built" != "$MODULE_SHA256" ]; then
	echo "oidc-fixture: the module built from $MODULE@$VERSION hashes to" >&2
	echo "  $built" >&2
	echo "and this pin names (there is no published release yet)" >&2
	echo "  $MODULE_SHA256" >&2
	echo "The two have to agree, or the suite is not testing what was published." >&2
	exit 1
fi

# The manifest, made the way the add-on's Makefile makes it: its addon.json.in
# with the module's real digest substituted. Doing it here rather than shipping a
# copy of addon.json means the manifest and the module cannot disagree, which is
# the one thing this host refuses at load.
mkdir -p "$OUT"
sed "s/@SHA256@/$built/" "$work/addon.json.in" >"$OUT/addon.json"
cp "$work/oidc.wasm" "$OUT/oidc.wasm"
# Kept beside the module so the suite can assert what this add-on consumes, and
# under what licence it may be consumed, without a network call of its own. The
# licence is an acceptance-test precondition rather than a courtesy: an
# unlicensed add-on repository means nobody may use the worked example M69 exists
# to produce.
cp "$work/go.mod" "$OUT/go.mod"
cp "$work/LICENSE" "$OUT/LICENSE"
printf '%s %s\n' "$MODULE" "$VERSION" >"$OUT/module.txt"

echo "oidc-fixture: $OUT/oidc.wasm sha256=$built ($MODULE@$VERSION)"

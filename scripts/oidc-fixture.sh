#!/usr/bin/env bash
#
# The OIDC add-on, as the integration suite's fixture (M69).
#
# M69's acceptance test needs the real add-on — `DevOfPie/LinkCtrl-OIDC`, which is
# a different repository and not this one's to build from a checkout. Two things
# are fetched and held against each other: the **source** at the released tag,
# from the module proxy, rebuilt the way the add-on's own Makefile builds it; and
# the **artifact** that release published, downloaded from the release itself. The
# fixture is the published artifact, and it is installed only once the rebuild has
# reproduced it byte for byte.
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
# **Why the Go toolchain is pinned too.** Go's compiler output moves between patch
# releases, so a rebuild that uses whatever `go` the machine happens to have
# proves something about the machine and nothing about the release. This module
# builds to MODULE_SHA256 below under go1.26.5 and to
# f917a810f6ac58cb35654a0e9c14bc86e7460b3d0775864648182e0f67cc393b under go1.26.7
# — both measured, which is F348, and which is why CI never ran an OIDC test. The
# pin is read from the module's own go.mod rather than transcribed here, because
# that is what cut the release: `LinkCtrl-OIDC`'s release workflow runs setup-go
# with `go-version-file: go.mod`. A tag's go.mod is immutable, so the value is as
# pinned as VERSION is and it moves with VERSION instead of being a fifth line to
# forget.
#
# **Why both digests are written here rather than read from the release.** They
# are what an operator types, and an operator who reads a digest off the page the
# link was on has authenticated nothing — which is what `docs/configuration.md`
# tells them and what this script therefore does. `SHA256SUMS` is not fetched:
# BUNDLE_SHA256 is the line it carries, transcribed once, and the download is
# checked against *that*. A release re-cut over the same tag fails here.
#
# Usage: scripts/oidc-fixture.sh [output-dir]
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

# The add-on, pinned at the release. Bumping this means bumping all four lines
# together: the tag, the bundle it published, and the two digests.
MODULE=github.com/DevOfPie/LinkCtrl-OIDC
VERSION=v0.1.0
BUNDLE=linkctrl-oidc-0.1.0.tar.gz
# The release's own SHA256SUMS line — the digest an operator types beside the URL
# in the Add-on manager, and what M68.6's install refuses to proceed without.
BUNDLE_SHA256=68f2d0c5794a042e28868efa2d01eb64fe56d97f888b08f04fdf0290d9515c02
# sha256 of the module the release published, which is also what its addon.json
# names and what M60's loader verifies before it instantiates anything.
# Reproduced by the build below; a mismatch is a refusal.
MODULE_SHA256=695385a1a796a689ad2b3b4f910f93c102adfe304728c5081ed215837cb4477e

BUNDLE_URL="https://github.com/DevOfPie/LinkCtrl-OIDC/releases/download/$VERSION/$BUNDLE"

OUT="${1:-test/integration/testdata/oidc}"

if [ -s "$OUT/oidc.wasm" ] && [ -s "$OUT/addon.json" ] && [ -s "$OUT/go.mod" ] &&
	[ -s "$OUT/LICENSE" ] && [ -s "$OUT/$BUNDLE" ]; then
	have=$(sha256sum "$OUT/oidc.wasm" | cut -d' ' -f1)
	haveBundle=$(sha256sum "$OUT/$BUNDLE" | cut -d' ' -f1)
	if [ "$have" = "$MODULE_SHA256" ] && [ "$haveBundle" = "$BUNDLE_SHA256" ]; then
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

# The toolchain the release was cut with, taken from the module's own go.mod the
# way setup-go's `go-version-file` takes it: a `toolchain` line when there is one,
# the `go` directive otherwise. GOTOOLCHAIN makes `go` fetch that toolchain from
# the proxy when the local one differs, so this holds on a machine that has never
# had it — which is the whole point, the fixture having been reproducible only on
# the machine that wrote the pin.
toolchain=$(sed -n 's/^toolchain[[:space:]]\{1,\}\(go[0-9][0-9.]*\)[[:space:]]*$/\1/p' "$work/go.mod" | head -n 1)
if [ -z "$toolchain" ]; then
	toolchain=$(sed -n 's/^go[[:space:]]\{1,\}\([0-9]\{1,\}\.[0-9]\{1,\}\.[0-9]\{1,\}\)[[:space:]]*$/go\1/p' "$work/go.mod" | head -n 1)
fi
if [ -z "$toolchain" ]; then
	echo "oidc-fixture: $MODULE@$VERSION names no exact Go toolchain in its go.mod," >&2
	echo "so the build below would use whatever this machine has and the digest it" >&2
	echo "produces would be a property of the machine rather than of the release." >&2
	echo "Pin the toolchain here before moving VERSION." >&2
	exit 1
fi

# The add-on's own build flags, and they are not decoration. `-buildvcs=false`
# stops Go stamping a VCS revision that would differ between a checkout and a
# module cache; `-trimpath` is what makes the bytes a function of the source
# alone; `-buildmode=c-shared` is the reactor shape this host instantiates.
echo "oidc-fixture: building the module for wasip1 with $toolchain"
(cd "$work" && GOTOOLCHAIN="$toolchain" GOOS=wasip1 GOARCH=wasm \
	go build -trimpath -buildvcs=false -buildmode=c-shared -o oidc.wasm .)

built=$(sha256sum "$work/oidc.wasm" | cut -d' ' -f1)
if [ "$built" != "$MODULE_SHA256" ]; then
	echo "oidc-fixture: the module built from $MODULE@$VERSION hashes to" >&2
	echo "  $built" >&2
	echo "and the release this pin names published" >&2
	echo "  $MODULE_SHA256" >&2
	echo "The two have to agree, or the suite is not testing what was published." >&2
	echo "It was built with $toolchain, read from the module's own go.mod. A digest" >&2
	echo "that differs only by toolchain is F348 again: check what cut the release." >&2
	exit 1
fi

# The manifest, made the way the add-on's Makefile makes it: its addon.json.in
# with the module's real digest substituted. It is built here so that the
# comparison below is against something this tree derived rather than against a
# copy of the answer.
sed "s/@SHA256@/$built/" "$work/addon.json.in" >"$work/addon.json.built"

# The artifact, from the release. Downloaded rather than reconstructed: the
# tarball's bytes depend on how it was packed and only the release knows that,
# while what has to be true — that it holds the module this source builds to — is
# checked directly a few lines down.
echo "oidc-fixture: downloading $BUNDLE"
if ! curl -fsSL --retry 3 -o "$work/$BUNDLE" "$BUNDLE_URL"; then
	echo "oidc-fixture: could not download $BUNDLE_URL" >&2
	echo "M69's acceptance test installs the artifact that release published, so this" >&2
	echo "download is not optional and there is no local substitute for it." >&2
	exit 1
fi

fetched=$(sha256sum "$work/$BUNDLE" | cut -d' ' -f1)
if [ "$fetched" != "$BUNDLE_SHA256" ]; then
	echo "oidc-fixture: the bundle at $BUNDLE_URL hashes to" >&2
	echo "  $fetched" >&2
	echo "and this pin names" >&2
	echo "  $BUNDLE_SHA256" >&2
	echo "Nothing was unpacked. A release re-cut over the same tag is one way to" >&2
	echo "reach this, and it is exactly what the digest is for." >&2
	exit 1
fi

# Unpacked into a directory of its own, so that what lands in $OUT is what the
# archive held and not a merge of the archive with the build.
mkdir -p "$work/bundle"
tar -xzf "$work/$BUNDLE" -C "$work/bundle"

# **The end-to-end check, and it is the point of this script.** The release
# published a bundle, the bundle carries a manifest and a module, the manifest
# names the module's digest, and the source the module proxy serves at this tag
# builds to that module. Every link is compared here rather than assumed, so a
# release built from something other than its own tag fails at the fixture step
# and not inside a test whose subject is OIDC.
for member in addon.json oidc.wasm; do
	if [ ! -s "$work/bundle/$member" ]; then
		echo "oidc-fixture: the published bundle holds no $member" >&2
		exit 1
	fi
done
if ! cmp -s "$work/bundle/oidc.wasm" "$work/oidc.wasm"; then
	echo "oidc-fixture: the module inside the published bundle is not the module" >&2
	echo "$MODULE@$VERSION builds to, so the release was not built from its own tag." >&2
	exit 1
fi
if ! cmp -s "$work/bundle/addon.json" "$work/addon.json.built"; then
	echo "oidc-fixture: the manifest inside the published bundle is not the manifest" >&2
	echo "this tag's addon.json.in produces for that module." >&2
	diff "$work/addon.json.built" "$work/bundle/addon.json" >&2 || true
	exit 1
fi

# What is installed is the published artifact's own bytes. The rebuild above is
# what earns the right to trust them; it is not what the suite runs.
mkdir -p "$OUT"
cp "$work/bundle/addon.json" "$OUT/addon.json"
cp "$work/bundle/oidc.wasm" "$OUT/oidc.wasm"
cp "$work/$BUNDLE" "$OUT/$BUNDLE"
# Kept beside the module so the suite can assert what this add-on consumes, and
# under what licence it may be consumed, without a network call of its own. The
# licence is an acceptance-test precondition rather than a courtesy: an
# unlicensed add-on repository means nobody may use the worked example M69 exists
# to produce.
cp "$work/go.mod" "$OUT/go.mod"
cp "$work/LICENSE" "$OUT/LICENSE"
printf '%s %s\n' "$MODULE" "$VERSION" >"$OUT/module.txt"

echo "oidc-fixture: $OUT/oidc.wasm sha256=$built ($MODULE@$VERSION)"
echo "oidc-fixture: $OUT/$BUNDLE sha256=$fetched, and it holds that module"

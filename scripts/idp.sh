#!/usr/bin/env bash
#
# The identity provider the integration suite signs a person in through (M69).
#
# M69 requires the OIDC add-on's flow to run against a real implementation of the
# protocol rather than a mock of it, and docker-compose.integration.yml is where
# that implementation is declared. This script is the two things compose cannot
# do for itself: make the certificate dex serves, and refuse to return until the
# provider actually answers.
#
# **Health-gating is the point of the second half.** m69.md names a containerized
# IdP as a new flake source and the single-flake budget (F256) as the lesson that
# applies, so `up` does not return on a started container. It waits for compose's
# own healthcheck *and* for the discovery document to parse, because those are two
# different claims: the first says the process is listening, the second says the
# thing the add-on is about to fetch is there. A suite that starts on the first
# and fails on the second is a flake nobody can reproduce.
#
# Usage: scripts/idp.sh up | down | issuer
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1

# `-p` is explicit and `--env-file` is not inherited, both deliberately. The
# Makefile exports COMPOSE_PROJECT_NAME and COMPOSE_ENV_FILES for the *instance*
# it is acting on, and an identity provider that joined `linkctrl-test` would be
# torn down by `make down` and built a second time under `INSTANCE=demo`. There is
# one of these, it belongs to the suite rather than to an instance, and naming the
# project here is what makes `scripts/idp.sh down` reach the same container
# whether make ran it or a person did.
COMPOSE=(docker compose -p linkctrl-idp --env-file /dev/null -f docker-compose.integration.yml)
TLS_DIR=test/integration/testdata/idp/tls
# Fixed, and it has to be: an issuer is compared byte for byte against the `iss`
# claim and it carries the port. See docker-compose.integration.yml.
ISSUER=https://127.0.0.1:5554/dex

usage() {
	echo "usage: $0 up | down | issuer" >&2
	exit 2
}

# certs writes the self-signed certificate dex serves, if it is not there.
#
# One certificate, self-signed, and it is its own root: the suite loads this exact
# file as the only additional root it trusts, so a separate CA would be two files
# to explain and no more assurance. `CA:TRUE` is what lets a chain of length one
# verify at all — Go refuses a chain whose root is not a CA — and the SAN is the
# loopback literal because the issuer is an address rather than a name, which
# keeps DNS out of a test that has enough moving parts already.
#
# Mode 0644 on the key is deliberate and is not carelessness: dex runs as uid 1001
# inside the container and cannot read a 0600 file this user owns. The key is
# generated locally, ignored by git, and trusted by nothing but the suite that
# made it.
certs() {
	if [ -s "$TLS_DIR/idp.crt" ] && [ -s "$TLS_DIR/idp.key" ]; then
		return 0
	fi
	mkdir -p "$TLS_DIR"
	openssl req -x509 -newkey rsa:2048 -sha256 -days 3650 -nodes \
		-keyout "$TLS_DIR/idp.key" -out "$TLS_DIR/idp.crt" \
		-subj "/CN=LinkCtrl integration IdP" \
		-addext "subjectAltName=IP:127.0.0.1,DNS:localhost" \
		-addext "basicConstraints=critical,CA:TRUE" \
		-addext "keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign" \
		-addext "extendedKeyUsage=serverAuth" >/dev/null 2>&1
	chmod 0644 "$TLS_DIR/idp.key" "$TLS_DIR/idp.crt"
	echo "idp: generated a self-signed certificate in $TLS_DIR"
}

# ready waits for the discovery document, which is the claim that matters.
ready() {
	local url="$ISSUER/.well-known/openid-configuration"
	local i out
	for i in $(seq 1 60); do
		if out=$(curl -sS --max-time 3 --cacert "$TLS_DIR/idp.crt" "$url" 2>/dev/null) &&
			grep -q '"token_endpoint"' <<<"$out"; then
			echo "idp: $ISSUER answered discovery after ${i} attempt(s)"
			return 0
		fi
		sleep 1
	done
	echo "idp: $url did not answer a discovery document within 60s" >&2
	"${COMPOSE[@]}" logs --no-color --tail 40 >&2 || true
	return 1
}

case "${1:-}" in
up)
	certs
	"${COMPOSE[@]}" up -d --wait
	ready
	;;
down)
	"${COMPOSE[@]}" down -v --remove-orphans
	;;
issuer)
	echo "$ISSUER"
	;;
*)
	usage
	;;
esac

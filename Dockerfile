# syntax=docker/dockerfile:1.7

# ─── Stage 1: stylesheet ─────────────────────────────────────────────────────
# Tailwind's standalone CLI is used rather than npm so that the production
# image contains no Node runtime. The version is pinned and the download is
# checksum-verified against the release's published sha256sums.txt, because
# this pulls a third-party binary into the build path.
FROM alpine:3.21 AS css
# The -musl Tailwind build is still dynamically linked against libstdc++ and
# libgcc, which bare Alpine does not ship; without them it fails at exec with
# "Error relocating ... symbol not found".
RUN apk add --no-cache curl libstdc++ libgcc
WORKDIR /src

ARG TAILWIND_VERSION=v4.1.14
ARG TARGETARCH

# The whole ui tree, not just input.css: Tailwind generates the stylesheet by
# scanning the templates (and funcs.go, which holds the badge classes) for
# class names. With only the entry point copied in, the scan finds nothing and
# the "successful" build ships a stylesheet containing just the preflight reset.
COPY internal/ui ./internal/ui

RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) asset="tailwindcss-linux-x64-musl" ;; \
        arm64) asset="tailwindcss-linux-arm64-musl" ;; \
        *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/tailwindlabs/tailwindcss/releases/download/${TAILWIND_VERSION}"; \
    curl -fsSL --retry 3 -o /tmp/tailwindcss "${base}/${asset}"; \
    curl -fsSL --retry 3 -o /tmp/sums.txt     "${base}/sha256sums.txt"; \
    # Entries are listed as "<hash>  ./<asset>", so match on a leading slash or
    # space rather than assuming either form.
    expected="$(grep -E "[ /]${asset}\$" /tmp/sums.txt | awk '{print $1}')"; \
    test -n "${expected}" || { echo "no checksum published for ${asset}" >&2; cat /tmp/sums.txt >&2; exit 1; }; \
    echo "${expected}  /tmp/tailwindcss" | sha256sum -c -; \
    chmod +x /tmp/tailwindcss; \
    /tmp/tailwindcss -i internal/ui/static/css/input.css -o /out/app.css --minify; \
    # A stylesheet that only contains the preflight reset means the @source
    # globs matched nothing — fail here, not in a browser three deploys later.
    test "$(wc -c < /out/app.css)" -gt 8192 || { echo "app.css is implausibly small; template scan found nothing" >&2; exit 1; }

# ─── Stage 2: build ──────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
COPY --from=css /out/app.css ./internal/ui/static/css/app.css

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ARG TARGETARCH

# CGO off so the result is a static binary that runs on a distroless base.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/DevOfPie/LinkCtrl/internal/build.version=${VERSION} \
        -X github.com/DevOfPie/LinkCtrl/internal/build.commit=${COMMIT} \
        -X github.com/DevOfPie/LinkCtrl/internal/build.date=${DATE}" \
      -o /out/linkctrl ./cmd/linkctrl \
 && CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags "-s -w" -o /out/lctl ./cmd/lctl

# ─── Stage 3: runtime ────────────────────────────────────────────────────────
# distroless/static: no shell, no package manager, no curl. That is why the
# compose healthcheck invokes the binary's own `healthcheck` subcommand rather
# than shelling out.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/linkctrl /linkctrl
COPY --from=build /out/lctl     /lctl

USER nonroot:nonroot
WORKDIR /
ENV TZ=UTC
EXPOSE 8080 9090

ENTRYPOINT ["/linkctrl"]

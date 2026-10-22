# syntax=docker/dockerfile:1
#
# Self-contained relay image: bakes in the pinned admin UI (embedded into the
# binary) and the pinned catalog data (seeded on first boot against an empty
# Postgres). A bare `docker run` with only RELAY_PG_DSN + RELAY_MASTER_KEY set
# comes up with a populated catalog and a working UI.
#
# Build args:
#   UI_VERSION      relay-ui release tag to embed
#   UI_SHA256       sha256 of that release's tarball
#   CATALOG_REF     relay-catalog release tag to seed (v0.1.15 or later)
#   CATALOG_SHA256  sha256 of that release's data tarball

# None of these has a literal default: versions.env pins them and
# docker-bake.hcl threads them through. A bare `docker build` must pass all
# four as --build-arg or the asset fetch fails loudly.
ARG UI_VERSION
ARG UI_SHA256
ARG CATALOG_REF
ARG CATALOG_SHA256

# --- assets: fetch pinned UI dist + catalog data; no toolchain reaches final ---
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS assets
RUN apk add --no-cache curl tar jq ca-certificates
ARG UI_VERSION
ARG UI_SHA256
ARG CATALOG_REF
ARG CATALOG_SHA256
WORKDIR /assets

# UI dist (relay-ui release tarball, public repo). A miss is a real error —
# a silent fallback here ships an API-only image that looks healthy until
# someone opens the admin UI. The pinned hash is the authority: the release's
# own .sha256 is served from the same place as the tarball, so it proves nothing.
RUN set -eu; \
  [ -n "${UI_SHA256}" ] || { echo "UI_SHA256 is not set; pin it in versions.env" >&2; exit 1; }; \
  mkdir -p /tmp/ui /assets/ui; \
  curl -fsSL -o /tmp/ui/ui.tar.gz "https://github.com/wyolet/relay-ui/releases/download/${UI_VERSION}/relay-ui-${UI_VERSION}.tar.gz"; \
  echo "${UI_SHA256}  /tmp/ui/ui.tar.gz" | sha256sum -c -; \
  tar -xz -C /assets/ui --strip-components=1 -f /tmp/ui/ui.tar.gz; \
  echo "fetched UI ${UI_VERSION}"

# Catalog data: the release's data tarball (data/ at its root, drafts left
# out), checked against the pinned hash for the same reason as the UI.
RUN set -eu; \
  [ -n "${CATALOG_REF}" ] || { echo "CATALOG_REF is not set; pin it in versions.env" >&2; exit 1; }; \
  [ -n "${CATALOG_SHA256}" ] || { echo "CATALOG_SHA256 is not set; pin it in versions.env" >&2; exit 1; }; \
  mkdir -p /tmp/catalog /assets/catalog; \
  curl -fsSL -o /tmp/catalog/catalog.tar.gz "https://github.com/wyolet/relay-catalog/releases/download/${CATALOG_REF}/relay-catalog-data-${CATALOG_REF}.tar.gz" \
    || { echo "relay-catalog release ${CATALOG_REF} has no relay-catalog-data-${CATALOG_REF}.tar.gz (CATALOG_REF must be a release tag, v0.1.15 or later)" >&2; exit 1; }; \
  echo "${CATALOG_SHA256}  /tmp/catalog/catalog.tar.gz" | sha256sum -c -; \
  tar -xz -C /assets/catalog --strip-components=1 -f /tmp/catalog/catalog.tar.gz data; \
  printf '%s\n' "${CATALOG_REF}" > /assets/catalog/.version; \
  echo "fetched catalog ${CATALOG_REF}"

# --- source: module cache + source tree, shared by the binary builds below ---
FROM golang:1.26-alpine@sha256:5cb67f19c5e7994dcb80c074f7cfca8aeeada45bbdca035f73df269cefdcce61 AS source
WORKDIR /src
# Copy the workspace + every locally-replaced module's go.mod/go.sum before
# download so the module-cache layer stays warm across source edits. go.mod
# replaces ./sdk, ./jobq, ./auth and ./secret, so their manifests must exist for `go mod download`.
COPY go.work go.work.sum ./
COPY go.mod go.sum ./
COPY sdk/go.mod sdk/go.sum ./sdk/
COPY jobq/go.mod jobq/go.sum ./jobq/
COPY auth/go.mod auth/go.sum ./auth/
COPY secret/go.mod secret/go.sum ./secret/
# GO_CACHE_MOUNTS=0 (published images, see docker-bake.hcl) skips the cache
# mounts: on a shared BuildKit any client can write them, and Go trusts cached
# modules and build objects without re-checking go.sum. Modules are then
# downloaded fresh (verified against go.sum) into a path no mount covers.
ARG GO_CACHE_MOUNTS=1
RUN --mount=type=cache,target=/go/pkg/mod \
    if [ "$GO_CACHE_MOUNTS" = 0 ]; then export GOMODCACHE=/gomod-private; fi; \
    go mod download
COPY . .

# --- builder: compile the binary with the UI embedded ---
FROM source AS builder
ARG GO_CACHE_MOUNTS=1
# Land the fetched UI (may be empty if no token + private repo). The .gitkeep
# guarantees dist/ is non-empty so `//go:embed all:dist` always compiles; an
# empty dist means Present() is false and no UI is served.
RUN mkdir -p cmd/relay/web/dist
COPY --from=assets /assets/ui/ cmd/relay/web/dist/
RUN touch cmd/relay/web/dist/.gitkeep
# VERSION stamps /api/version + the OpenAPI Info block; "dev" when unset
# (bare `docker build`). The release pipeline passes the git tag via
# docker-bake.hcl.
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "$GO_CACHE_MOUNTS" = 0 ]; then export GOMODCACHE=/gomod-private GOCACHE=/tmp/go-build-private; fi; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/wyolet/relay/app/httpapi.Version=${VERSION}" \
    -o /relay ./cmd/relay

# --- catalog-builder: catalog-server + catalog-validate; no UI or catalog assets ---
FROM source AS catalog-builder
ARG GO_CACHE_MOUNTS=1
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    if [ "$GO_CACHE_MOUNTS" = 0 ]; then export GOMODCACHE=/gomod-private GOCACHE=/tmp/go-build-private; fi; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/catalog-server ./cmd/catalog-validate

# --- final (lean): distroless, nonroot — production image, external Postgres ---
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS lean
LABEL org.opencontainers.image.source="https://github.com/wyolet/relay"
COPY --from=builder /relay /relay
COPY --from=assets /assets/catalog /catalog
# Self-seed the embedded catalog on first boot (empty PG only; never clobbers).
ENV RELAY_CATALOG_DIR=/catalog \
    RELAY_AUTO_SEED_IF_EMPTY=1
EXPOSE 8080 8081
ENTRYPOINT ["/relay"]

# --- standalone: relay + embedded Postgres in one container — `docker run` demo ---
# Single-node convenience image. Boots a local Postgres (initdb on first run)
# then relay against it, so no external services are needed. Built on alpine +
# the apk postgresql16 package (no LLVM JIT) — a fraction of the official
# postgres image's size. Single-node only; production uses the lean image above
# against managed PG.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS standalone
LABEL org.opencontainers.image.source="https://github.com/wyolet/relay"
RUN apk add --no-cache postgresql16 postgresql16-client tzdata \
    && mkdir -p /var/lib/postgresql/data /run/postgresql \
    && chown -R postgres:postgres /var/lib/postgresql /run/postgresql
COPY --from=builder /relay /relay
COPY --from=assets /assets/catalog /catalog
COPY deploy/standalone-entrypoint.sh /usr/local/bin/relay-standalone-entrypoint.sh
RUN chmod +x /usr/local/bin/relay-standalone-entrypoint.sh
ENV RELAY_CATALOG_DIR=/catalog \
    RELAY_AUTO_SEED_IF_EMPTY=1 \
    POSTGRES_USER=relay \
    POSTGRES_DB=relay \
    PGDATA=/var/lib/postgresql/data \
    RELAY_PG_DSN=postgres://relay:relay@127.0.0.1:5432/relay?sslmode=disable \
    RELAY_CONTROL_PORT=8081 \
    RELAY_INFERENCE_API_URL=http://localhost:8080 \
    RELAY_COOKIE_SECURE=false
# Demo defaults so a bare `docker run -p 8080:8080 -p 8081:8081` just works:
#   - RELAY_INFERENCE_API_URL pins the data-plane origin the embedded UI calls
#     (served via GET /config.json); empty would leave the UI with no endpoint.
#   - RELAY_COOKIE_SECURE=false lets the session cookie flow over plain-HTTP
#     localhost (UI login). The master key + admin token are auto-generated by
#     the entrypoint on first boot. Override any of these with `-e` for a real
#     deployment (e.g. a public URL + RELAY_COOKIE_SECURE=true behind TLS).
# Only relay's two HTTP planes. Postgres is internal (loopback only) and never
# exposed — use `docker exec` if you need to poke at it.
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/relay-standalone-entrypoint.sh"]

# --- catalog-server: serves relay-catalog releases; also the catalog repo's CI tools ---
# git is required: release data tarballs are `git archive` output. No ENTRYPOINT,
# so `docker run <image> catalog-validate ...` runs either binary.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS catalog-server
LABEL org.opencontainers.image.source="https://github.com/wyolet/relay"
RUN apk add --no-cache git \
    && addgroup -S -g 65532 catalog \
    && adduser -S -D -H -u 65532 -G catalog catalog
COPY --from=catalog-builder /out/catalog-server /out/catalog-validate /usr/local/bin/
USER 65532:65532
EXPOSE 8080
CMD ["catalog-server", "serve"]

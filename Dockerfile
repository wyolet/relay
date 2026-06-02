# syntax=docker/dockerfile:1
#
# Self-contained relay image: bakes in the pinned admin UI (embedded into the
# binary) and the pinned catalog data (seeded on first boot against an empty
# Postgres). A bare `docker run` with only RELAY_PG_DSN + RELAY_MASTER_KEY set
# comes up with a populated catalog and a working UI.
#
# Build args:
#   UI_VERSION   relay-ui release tag to embed (default below)
#   CATALOG_REF  relay-catalog git ref (tag/branch/sha) to seed (default below)
# Build secret (optional):
#   gh_token     GitHub token for fetching the relay-ui release while that repo
#                is private. Omit once relay-ui is public.
#     docker buildx build --secret id=gh_token,env=GH_TOKEN ...

ARG UI_VERSION=v0.0.1
ARG CATALOG_REF=main

# --- assets: fetch pinned UI dist + catalog data; no toolchain reaches final ---
FROM alpine:3.20 AS assets
RUN apk add --no-cache curl tar jq ca-certificates
ARG UI_VERSION
ARG CATALOG_REF
WORKDIR /assets

# UI dist (relay-ui release tarball). With a gh_token secret the GitHub API
# asset endpoint authenticates against the still-private repo; failure is then
# fatal (you have access — a miss is a real error). Without a token the build
# is best-effort: relay-ui is private, so the fetch will miss and the image
# ships with an empty dist (relay boots API-only; UI not served). This keeps a
# tokenless `docker compose up --build` working for strangers today; once
# relay-ui is public the tokenless path fetches it too. The published image is
# built WITH a token, so it always carries the UI.
RUN --mount=type=secret,id=gh_token,required=false sh -eu -c '\
  REPO=wyolet/relay-ui; TAG='"$UI_VERSION"'; ASSET="relay-ui-$TAG.tar.gz"; \
  TOKEN=$(cat /run/secrets/gh_token 2>/dev/null || true); \
  mkdir -p /assets/ui; \
  if [ -n "$TOKEN" ]; then \
    AID=$(curl -fsSL -H "Authorization: Bearer $TOKEN" \
            "https://api.github.com/repos/$REPO/releases/tags/$TAG" \
          | jq -r ".assets[] | select(.name==\"$ASSET\") | .id"); \
    [ -n "$AID" ] || { echo "FATAL: asset $ASSET not found in $REPO@$TAG"; exit 1; }; \
    curl -fsSL -H "Authorization: Bearer $TOKEN" -H "Accept: application/octet-stream" \
      "https://api.github.com/repos/$REPO/releases/assets/$AID" \
      | tar -xz -C /assets/ui --strip-components=1; \
  elif curl -fsSL "https://github.com/$REPO/releases/download/$TAG/$ASSET" \
         | tar -xz -C /assets/ui --strip-components=1; then \
    echo "fetched UI $TAG (public)"; \
  else \
    echo "WARN: no gh_token and UI fetch failed (relay-ui private?) — building without embedded UI"; \
  fi'

# Catalog data (public repo). No release tarballs yet, so fetch the source
# archive at the pinned ref and keep only the live data tree (drafts/ are
# skipped by the seed loader anyway — dropped here to keep the image minimal).
RUN set -eu; \
  curl -fsSL "https://github.com/wyolet/relay-catalog/archive/${CATALOG_REF}.tar.gz" \
    | tar -xz -C /tmp; \
  mkdir -p /assets/catalog; \
  mv /tmp/relay-catalog-*/data/* /assets/catalog/; \
  rm -rf /assets/catalog/drafts

# --- builder: compile the binary with the UI embedded ---
FROM golang:1.25-alpine AS builder
WORKDIR /src
# Copy the workspace + every locally-replaced module's go.mod/go.sum before
# download so the module-cache layer stays warm across source edits. go.mod
# replaces ./sdk and ./jobq, so their manifests must exist for `go mod download`.
COPY go.work go.work.sum ./
COPY go.mod go.sum ./
COPY sdk/go.mod sdk/go.sum ./sdk/
COPY jobq/go.mod jobq/go.sum ./jobq/
RUN go mod download
COPY . .
# Land the fetched UI (may be empty if no token + private repo). The .gitkeep
# guarantees dist/ is non-empty so `//go:embed all:dist` always compiles; an
# empty dist means Present() is false and no UI is served.
RUN mkdir -p cmd/relay/web/dist
COPY --from=assets /assets/ui/ cmd/relay/web/dist/
RUN touch cmd/relay/web/dist/.gitkeep
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /relay ./cmd/relay

# --- final (lean): distroless, nonroot — production image, external Postgres ---
FROM gcr.io/distroless/static-debian12:nonroot AS lean
LABEL org.opencontainers.image.source="https://github.com/wyolet/relay"
COPY --from=builder /relay /relay
COPY --from=assets /assets/catalog /catalog
# Self-seed the embedded catalog on first boot (empty PG only; never clobbers).
ENV RELAY_CATALOG_DIR=/catalog \
    RELAY_AUTO_SEED_IF_EMPTY=1
EXPOSE 8080 8081
ENTRYPOINT ["/relay"]

# --- allinone: relay + embedded Postgres in one container — `docker run` demo ---
# Single-node convenience image. Boots a local Postgres (initdb on first run)
# then relay against it, so no external services are needed. Heavier and
# single-node only — production uses the lean image above against managed PG.
FROM postgres:16-alpine AS allinone
LABEL org.opencontainers.image.source="https://github.com/wyolet/relay"
COPY --from=builder /relay /relay
COPY --from=assets /assets/catalog /catalog
COPY deploy/allinone-entrypoint.sh /usr/local/bin/relay-allinone-entrypoint.sh
RUN chmod +x /usr/local/bin/relay-allinone-entrypoint.sh
ENV RELAY_CATALOG_DIR=/catalog \
    RELAY_AUTO_SEED_IF_EMPTY=1 \
    POSTGRES_USER=relay \
    POSTGRES_PASSWORD=relay \
    POSTGRES_DB=relay \
    PGDATA=/var/lib/postgresql/data \
    RELAY_PG_DSN=postgres://relay:relay@127.0.0.1:5432/relay?sslmode=disable
EXPOSE 8080 8081 5432
ENTRYPOINT ["/usr/local/bin/relay-allinone-entrypoint.sh"]

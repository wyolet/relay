// wyolet relay — docker buildx bake config

variable "REGISTRY"     { default = "ghcr.io/wyolet" }
variable "DOCKERHUB"    { default = "docker.io/wyolet" }
variable "IMAGE_NAME"   { default = "relay" }
variable "VERSION"      { default = "latest" }
variable "GIT_REVISION" { default = "" }
# UI_VERSION is the single source of truth in the Makefile (UI_VERSION ?= ...);
# `make image`/`release`/etc. pass it through as env. The empty default here is
# only a fallback for a bare `docker buildx bake` — which will then fail the UI
# fetch loudly rather than silently embedding the wrong version. Bump it in the
# Makefile, nowhere else.
variable "UI_VERSION"   { default = "" }
variable "CATALOG_REF"  { default = "main" }

target "_common" {
  context    = "."
  dockerfile = "Dockerfile"
  target     = "lean"
  platforms  = ["linux/amd64", "linux/arm64"]
  args = {
    UI_VERSION  = "${UI_VERSION}"
    CATALOG_REF = "${CATALOG_REF}"
  }
  // UI fetch token for the (private) relay-ui repo, read from the GH_TOKEN env.
  // Optional in the Dockerfile (required=false) — unset env yields an empty
  // secret and a UI-less build; CI sets it to embed the UI.
  secret = ["id=gh_token,env=GH_TOKEN"]
  // Always rebuild the asset-fetch stage: BuildKit excludes secret VALUES from
  // the cache key, so a cached `assets` layer could reuse a stale (e.g. UI-less)
  // fetch. This also guarantees the pinned UI/catalog are re-pulled each build.
  no-cache-filter = ["assets"]
}

// Production: pushes the lean image as :VERSION + :latest + :sha to both
// registries (ghcr + Docker Hub).
target "prod" {
  inherits    = ["_common"]
  description = "Lean multi-arch production image (external Postgres); pushes :VERSION + :latest + :sha to ghcr + Docker Hub"
  tags = compact([
    "${REGISTRY}/${IMAGE_NAME}:${VERSION}",
    "${REGISTRY}/${IMAGE_NAME}:latest",
    notequal("", GIT_REVISION) ? "${REGISTRY}/${IMAGE_NAME}:${GIT_REVISION}" : "",
    "${DOCKERHUB}/${IMAGE_NAME}:${VERSION}",
    "${DOCKERHUB}/${IMAGE_NAME}:latest",
    notequal("", GIT_REVISION) ? "${DOCKERHUB}/${IMAGE_NAME}:${GIT_REVISION}" : "",
  ])
}

// Development: separate moving label so dev pushes don't move :latest.
target "dev" {
  inherits    = ["_common"]
  description  = "Lean image on the :dev moving label (+ :sha); dev pushes don't move :latest"
  tags = compact([
    "${REGISTRY}/${IMAGE_NAME}:dev",
    notequal("", GIT_REVISION) ? "${REGISTRY}/${IMAGE_NAME}:${GIT_REVISION}" : "",
  ])
}

// Standalone: relay + embedded Postgres (Dockerfile `standalone` stage). The
// `docker run` image, published as :standalone (+ :VERSION-standalone).
target "standalone" {
  inherits    = ["_common"]
  description = "Standalone image (relay + embedded Postgres) for `docker run`; pushes :standalone + :VERSION-standalone to ghcr + Docker Hub"
  target      = "standalone"
  tags = compact([
    "${REGISTRY}/${IMAGE_NAME}:standalone",
    notequal("latest", VERSION) ? "${REGISTRY}/${IMAGE_NAME}:${VERSION}-standalone" : "",
    "${DOCKERHUB}/${IMAGE_NAME}:standalone",
    notequal("latest", VERSION) ? "${DOCKERHUB}/${IMAGE_NAME}:${VERSION}-standalone" : "",
  ])
}

// Local: load into the local docker daemon for smoke testing. Host-native
// (no platforms list — the docker exporter can't do multi-arch). Repeats the
// args + secret rather than inheriting _common's multi-platform build.
target "local" {
  description = "Lean image built host-native into the local docker daemon as relay:dev (smoke testing)"
  context     = "."
  dockerfile  = "Dockerfile"
  target      = "lean"
  output      = ["type=docker"]
  tags        = ["${IMAGE_NAME}:dev"]
  args = {
    UI_VERSION  = "${UI_VERSION}"
    CATALOG_REF = "${CATALOG_REF}"
  }
  secret = ["id=gh_token,env=GH_TOKEN"]
}

// Local standalone, for smoke-testing `docker run`.
target "local-standalone" {
  description = "Standalone image built host-native into the local docker daemon as relay:standalone (smoke testing `docker run`)"
  context     = "."
  dockerfile  = "Dockerfile"
  target      = "standalone"
  output      = ["type=docker"]
  tags        = ["${IMAGE_NAME}:standalone"]
  args = {
    UI_VERSION  = "${UI_VERSION}"
    CATALOG_REF = "${CATALOG_REF}"
  }
  secret = ["id=gh_token,env=GH_TOKEN"]
}

group "all"     { targets = ["prod", "dev", "standalone"] }
# release = the two PUBLISHED artifacts that must stay in lockstep every version:
# the lean image (:VERSION/:latest) and the standalone image (:standalone).
# Excludes the :dev moving label. `make release`/`image` bake this so :standalone
# never goes stale behind a fresh :latest.
group "release" { targets = ["prod", "standalone"] }
group "default" { targets = ["prod"] }

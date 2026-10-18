// wyolet relay — docker buildx bake config

variable "REGISTRY"     { default = "ghcr.io/wyolet" }
variable "DOCKERHUB"    { default = "docker.io/wyolet" }
variable "IMAGE_NAME"   { default = "relay" }
variable "CATALOG_SERVER_IMAGE_NAME" { default = "catalog-server" }
variable "VERSION"      { default = "latest" }
variable "GIT_REVISION" { default = "" }
# UI_VERSION, UI_SHA256 + CATALOG_REF are pinned in versions.env (the release
# BOM); the Makefile `include`s it and passes them through as env on every bake
# call. Bump them in versions.env, nowhere else. The defaults here are only
# fallbacks for a bare `docker buildx bake` with no Makefile: an empty
# UI_VERSION or UI_SHA256 fails the UI fetch loudly rather than embedding an
# unverified UI; CATALOG_REF="main" tracks the branch (non-reproducible) and is
# why releases must go through Make.
variable "UI_VERSION"   { default = "" }
variable "UI_SHA256"    { default = "" }
variable "CATALOG_REF"  { default = "main" }

target "_common" {
  context    = "."
  dockerfile = "Dockerfile"
  target     = "lean"
  // arm64 disabled while the shared builder has no native arm node — the
  // emulated leg dominates release time; arm images are built natively on
  // an arm machine instead.
  platforms  = ["linux/amd64"] // , "linux/arm64"
  args = {
    UI_VERSION  = "${UI_VERSION}"
    UI_SHA256   = "${UI_SHA256}"
    CATALOG_REF = "${CATALOG_REF}"
    // Binary version stamp (/api/version). "latest" is a tag alias, not a
    // version — unversioned builds stamp "dev".
    VERSION = notequal("latest", VERSION) ? "${VERSION}" : "dev"
    // Published images never read the shared Go cache mounts (see Dockerfile).
    GO_CACHE_MOUNTS = "0"
  }
  // Always rebuild the asset-fetch stage so the pinned UI/catalog are
  // re-pulled each build rather than served from a stale cached layer.
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

// catalog-server (Dockerfile `catalog-server` stage), versioned with relay;
// same tags as prod.
target "catalog-server" {
  inherits    = ["_common"]
  description = "catalog-server image (catalog-server + catalog-validate + git); pushes :VERSION + :latest + :sha to ghcr + Docker Hub"
  target      = "catalog-server"
  tags = compact([
    "${REGISTRY}/${CATALOG_SERVER_IMAGE_NAME}:${VERSION}",
    "${REGISTRY}/${CATALOG_SERVER_IMAGE_NAME}:latest",
    notequal("", GIT_REVISION) ? "${REGISTRY}/${CATALOG_SERVER_IMAGE_NAME}:${GIT_REVISION}" : "",
    "${DOCKERHUB}/${CATALOG_SERVER_IMAGE_NAME}:${VERSION}",
    "${DOCKERHUB}/${CATALOG_SERVER_IMAGE_NAME}:latest",
    notequal("", GIT_REVISION) ? "${DOCKERHUB}/${CATALOG_SERVER_IMAGE_NAME}:${GIT_REVISION}" : "",
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
    UI_SHA256   = "${UI_SHA256}"
    CATALOG_REF = "${CATALOG_REF}"
    // Binary version stamp (/api/version). "latest" is a tag alias, not a
    // version — unversioned builds stamp "dev".
    VERSION = notequal("latest", VERSION) ? "${VERSION}" : "dev"
  }
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
    UI_SHA256   = "${UI_SHA256}"
    CATALOG_REF = "${CATALOG_REF}"
    // Binary version stamp (/api/version). "latest" is a tag alias, not a
    // version — unversioned builds stamp "dev".
    VERSION = notequal("latest", VERSION) ? "${VERSION}" : "dev"
  }
}

// Local catalog-server, for smoke-testing serve/build/validate.
target "local-catalog-server" {
  description = "catalog-server image built host-native into the local docker daemon as catalog-server:dev"
  context     = "."
  dockerfile  = "Dockerfile"
  target      = "catalog-server"
  output      = ["type=docker"]
  tags        = ["${CATALOG_SERVER_IMAGE_NAME}:dev"]
}

group "all"     { targets = ["prod", "dev", "standalone", "catalog-server"] }
# release = the PUBLISHED artifacts that must stay in lockstep every version:
# the lean image (:VERSION/:latest), the standalone image (:standalone) and the
# catalog-server image. Excludes the :dev moving label. `make release`/`image`
# bake this so :standalone never goes stale behind a fresh :latest.
group "release" { targets = ["prod", "standalone", "catalog-server"] }
group "default" { targets = ["prod"] }

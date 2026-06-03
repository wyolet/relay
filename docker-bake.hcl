// wyolet relay — docker buildx bake config

variable "REGISTRY"     { default = "ghcr.io/wyolet" }
variable "DOCKERHUB"    { default = "docker.io/wyolet" }
variable "IMAGE_NAME"   { default = "relay" }
variable "VERSION"      { default = "latest" }
variable "GIT_REVISION" { default = "" }
variable "UI_VERSION"   { default = "v0.2.1" }
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

// All-in-one: relay + embedded Postgres (Dockerfile `allinone` stage). The
// `docker run` image, published as :standalone (+ :VERSION-standalone).
target "allinone" {
  inherits    = ["_common"]
  description = "All-in-one image (relay + embedded Postgres) for `docker run`; pushes :standalone + :VERSION-standalone to ghcr + Docker Hub"
  target      = "allinone"
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

// Local all-in-one, for smoke-testing `docker run`.
target "local-allinone" {
  description = "All-in-one image built host-native into the local docker daemon as relay:allinone (smoke testing `docker run`)"
  context     = "."
  dockerfile  = "Dockerfile"
  target      = "allinone"
  output      = ["type=docker"]
  tags        = ["${IMAGE_NAME}:allinone"]
  args = {
    UI_VERSION  = "${UI_VERSION}"
    CATALOG_REF = "${CATALOG_REF}"
  }
  secret = ["id=gh_token,env=GH_TOKEN"]
}

group "all"     { targets = ["prod", "dev", "allinone"] }
group "default" { targets = ["prod"] }

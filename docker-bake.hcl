// wyolet relay — docker buildx bake config

variable "REGISTRY"     { default = "ghcr.io/wyolet" }
variable "IMAGE_NAME"   { default = "relay" }
variable "VERSION"      { default = "latest" }
variable "GIT_REVISION" { default = "" }
variable "UI_VERSION"   { default = "v0.0.1" }
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
}

// Production: pushes :$(VERSION) + :latest + :$(GIT_REVISION) to the registry.
target "prod" {
  inherits    = ["_common"]
  description  = "Lean multi-arch production image (external Postgres); pushes :VERSION + :latest + :sha"
  tags = compact([
    "${REGISTRY}/${IMAGE_NAME}:${VERSION}",
    "${REGISTRY}/${IMAGE_NAME}:latest",
    notequal("", GIT_REVISION) ? "${REGISTRY}/${IMAGE_NAME}:${GIT_REVISION}" : "",
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
// `docker run` demo image. Tag scheme TBD — provisionally :demo + version.
target "allinone" {
  inherits    = ["_common"]
  description  = "All-in-one demo image (relay + embedded Postgres) for `docker run`; pushes :demo + :VERSION-allinone"
  target      = "allinone"
  tags = compact([
    "${REGISTRY}/${IMAGE_NAME}:demo",
    notequal("latest", VERSION) ? "${REGISTRY}/${IMAGE_NAME}:${VERSION}-allinone" : "",
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

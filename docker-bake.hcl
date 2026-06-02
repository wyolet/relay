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

// Production: pushes :$(VERSION) + :latest + :$(GIT_REVISION) to Harbor.
target "prod" {
  inherits = ["_common"]
  tags = compact([
    "${REGISTRY}/${IMAGE_NAME}:${VERSION}",
    "${REGISTRY}/${IMAGE_NAME}:latest",
    notequal("", GIT_REVISION) ? "${REGISTRY}/${IMAGE_NAME}:${GIT_REVISION}" : "",
  ])
}

// Development: separate moving label so dev pushes don't move :latest.
target "dev" {
  inherits = ["_common"]
  tags = compact([
    "${REGISTRY}/${IMAGE_NAME}:dev",
    notequal("", GIT_REVISION) ? "${REGISTRY}/${IMAGE_NAME}:${GIT_REVISION}" : "",
  ])
}

// Local: load into the local docker daemon for smoke testing. Host-native
// (no platforms list — the docker exporter can't do multi-arch). Repeats the
// args + secret rather than inheriting _common's multi-platform build.
target "local" {
  context    = "."
  dockerfile = "Dockerfile"
  output     = ["type=docker"]
  tags       = ["${IMAGE_NAME}:dev"]
  args = {
    UI_VERSION  = "${UI_VERSION}"
    CATALOG_REF = "${CATALOG_REF}"
  }
  secret = ["id=gh_token,env=GH_TOKEN"]
}

group "all"     { targets = ["prod", "dev"] }
group "default" { targets = ["prod"] }

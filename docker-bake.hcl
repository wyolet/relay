// wyolet relay — docker buildx bake config

variable "REGISTRY"     { default = "ghcr.io/wyolet" }
variable "IMAGE_NAME"   { default = "relay" }
variable "VERSION"      { default = "latest" }
variable "GIT_REVISION" { default = "" }

target "_common" {
  context    = "."
  dockerfile = "Dockerfile"
  platforms  = ["linux/amd64", "linux/arm64"]
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

// Local: load into the local docker daemon for smoke testing.
target "local" {
  context    = "."
  dockerfile = "Dockerfile"
  output     = ["type=docker"]
  tags       = ["${IMAGE_NAME}:dev"]
}

group "all"     { targets = ["prod", "dev"] }
group "default" { targets = ["prod"] }

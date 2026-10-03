// Builds the ntindle fork image: the repository's own Dockerfile first, then the runtime
// layer in docker/fork/Dockerfile on top of it. Run from the repository root:
//
//   docker buildx bake -f docker/fork/docker-bake.hcl --load
//
// Relative paths below are resolved from the directory bake is invoked in.

variable "IMAGE" {
  default = "ghcr.io/ntindle/cliproxyapi"
}

// Comma-separated image tags.
variable "TAGS" {
  default = "dev"
}

variable "VERSION" {
  default = "dev"
}

variable "COMMIT" {
  default = "none"
}

variable "BUILD_DATE" {
  default = "unknown"
}

group "default" {
  targets = ["fork"]
}

target "upstream" {
  context    = "."
  dockerfile = "Dockerfile"
  args = {
    VERSION    = VERSION
    COMMIT     = COMMIT
    BUILD_DATE = BUILD_DATE
  }
}

target "fork" {
  context    = "."
  dockerfile = "docker/fork/Dockerfile"
  contexts = {
    cliproxyapi-upstream = "target:upstream"
  }
  tags = [for tag in split(",", TAGS) : "${IMAGE}:${trimspace(tag)}"]
  labels = {
    "org.opencontainers.image.title"       = "CLIProxyAPI (ntindle fork)"
    "org.opencontainers.image.description" = "CLIProxyAPI with soonest-reset routing, live Claude and Codex model discovery, and a single /data volume."
    "org.opencontainers.image.source"      = "https://github.com/ntindle/CLIProxyAPI"
    "org.opencontainers.image.url"         = "https://github.com/ntindle/CLIProxyAPI"
    "org.opencontainers.image.licenses"    = "MIT"
    "org.opencontainers.image.version"     = VERSION
    "org.opencontainers.image.revision"    = COMMIT
    "org.opencontainers.image.created"     = BUILD_DATE
  }
}

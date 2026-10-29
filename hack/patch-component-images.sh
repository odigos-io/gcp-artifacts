#!/usr/bin/env bash
# Builds patched versions of the official Odigos component images that the Marketplace vulnerability
# scan flags, and publishes them like hack/publish-component-images.sh does (annotation + tags).
# Run publish-component-images.sh first; this overwrites the tags of the patched components only.
#
# Usage: hack/patch-component-images.sh 1.39.0 <path to dtrace-injector.jar>
#   The jar replaces /instrumentations/java-ebpf/dtrace-injector.jar in the enterprise images; it must be
#   built from the same java-ebpf version the release ships.
set -euo pipefail

VERSION="${1:?usage: $0 <version> <dtrace-injector.jar>}"
JAR="${2:?usage: $0 <version> <dtrace-injector.jar>}"
VERSION="${VERSION#v}"
TRACK="${VERSION%.*}"
SRC="${SRC:-registry.odigos.io}"
DEST="${DEST:-gcr.io/odigos-public/odigos}"
SERVICE_NAME="services/odigos.endpoints.odigos-public.cloud.goog"
HERE="$(cd "$(dirname "$0")" && pwd)"

ctx="$(mktemp -d)"
trap 'rm -rf "$ctx"' EXIT
cp "$JAR" "$ctx/dtrace-injector.jar"

# component  patch-grpc  patch-java
PATCHES=(
  "odiglet            yes no"
  "agents             no  no"
  "enterprise-odiglet no  yes"
  "enterprise-agents  no  yes"
)

for p in "${PATCHES[@]}"; do
  read -r c grpc java <<<"$p"
  src="${SRC}/odigos-${c}:v${VERSION}"
  base="${src}@$(crane digest "$src")"
  echo "==> ${c}: ${base} (grpc=${grpc} java=${java})"
  {
    cat "$HERE/patch-component-images.Dockerfile"
    crane config --platform linux/amd64 "$src" | python3 "$HERE/image-config-stage.py"
  } > "$ctx/Dockerfile"
  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --provenance=false --sbom=false \
    --build-arg BASE="$base" \
    --build-arg PATCH_GRPC="$grpc" \
    --build-arg PATCH_JAVA="$java" \
    --annotation "index:com.googleapis.cloudmarketplace.product.service.name=${SERVICE_NAME}" \
    --tag "${DEST}/odigos-${c}:v${VERSION}" \
    --tag "${DEST}/odigos-${c}:${VERSION}" \
    --tag "${DEST}/odigos-${c}:${TRACK}" \
    --output type=image,push=true,oci-mediatypes=true \
    "$ctx"
  crane manifest "${DEST}/odigos-${c}:${VERSION}" | grep -q 'cloudmarketplace.product.service.name' \
    || { echo "ERROR: ${DEST}/odigos-${c}:${VERSION} is missing the Marketplace annotation" >&2; exit 1; }
done

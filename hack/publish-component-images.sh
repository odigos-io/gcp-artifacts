#!/usr/bin/env bash
# Publishes the Odigos component images referenced by schema.yaml to the Marketplace staging registry.
#
# Instead of rebuilding from source, each official release image index (registry.odigos.io) is re-indexed
# into $DEST with the Marketplace service-name annotation on the index. The per-platform manifests are the
# exact official release images; only the top-level index differs.
#
# Usage: hack/publish-component-images.sh 1.39.0
set -euo pipefail

VERSION="${1:?usage: $0 <version, e.g. 1.39.0>}"
VERSION="${VERSION#v}"
TRACK="${VERSION%.*}"
SRC="${SRC:-registry.odigos.io}"
DEST="${DEST:-gcr.io/odigos-public/odigos}"
SERVICE_NAME="services/odigos.endpoints.odigos-public.cloud.goog"

# Must match the images declared in schema.yaml (x-google-marketplace.images).
COMPONENTS=(
  autoscaler collector instrumentor odiglet ui agents scheduler
  enterprise-agents enterprise-odiglet enterprise-instrumentor
)

for c in "${COMPONENTS[@]}"; do
  src="${SRC}/odigos-${c}:v${VERSION}"
  dst="${DEST}/odigos-${c}"
  echo "==> ${src} -> ${dst}:{v${VERSION},${VERSION},${TRACK}}"
  docker buildx imagetools create \
    --annotation "index:com.googleapis.cloudmarketplace.product.service.name=${SERVICE_NAME}" \
    --tag "${dst}:v${VERSION}" \
    --tag "${dst}:${VERSION}" \
    --tag "${dst}:${TRACK}" \
    "${src}"
done

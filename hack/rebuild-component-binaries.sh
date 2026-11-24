#!/usr/bin/env bash
# Rebuilds the Go binaries of official Odigos component images with a newer Go toolchain and swaps them into
# the official images, for Go stdlib CVEs that the Marketplace vulnerability scan flags. Everything else in
# the images (bundled agents, base layers, runtime config) stays as officially released.
#
# Each binary is built by the component's own Dockerfile from the Odigos release tag, with only its golang
# builder image bumped. The new binaries are copied over the official image, and the result is flattened into
# one layer (so the old binaries don't survive in a lower layer) with the original image config restored, then
# published like hack/publish-component-images.sh does (annotated OCI index, tags v$VERSION, $VERSION, $TRACK).
# Run it after publish-component-images.sh: it overwrites the tags of the components it rebuilds.
#
# Usage: hack/rebuild-component-binaries.sh <odigos checkout> <component>...
#   <odigos checkout>  a clean checkout (or worktree) of odigos-io/odigos at tag v$VERSION
#   <component>        enterprise-odiglet, enterprise-instrumentor, enterprise-collector, enterprise-ui, cli
# Environment:
#   VERSION       Odigos version (required, e.g. 1.40.0)
#   GO_VERSION    Go toolchain for the rebuilt binaries (required, e.g. 1.26.11)
#   ODIGLET_BASE_IMAGE  odiglet builder base built from odiglet/base.Dockerfile with GO_VERSION
#                       (required for enterprise-odiglet; see RELEASING.md)
#   GRPC_HEALTH_PROBE_VERSION  grpc_health_probe release for enterprise-odiglet (default: the Dockerfile's)
#   GITHUB_TOKEN  token that can read the private modules enterprise-odiglet builds with (enterprise-odiglet)
#   NPMRC         .npmrc with a GitHub Packages token for @odigos-io/ui-kit (enterprise-ui, default ~/.npmrc)
#   DEST          destination registry path (default gcr.io/odigos-public/odigos-staging/final)
#   SRC           registry of the official images (default registry.odigos.io)
set -euo pipefail

CHECKOUT="$(cd "${1:?usage: $0 <odigos checkout> <component>...}" && pwd)"
shift
[ $# -gt 0 ] || { echo "usage: $0 <odigos checkout> <component>..." >&2; exit 1; }
VERSION="${VERSION:?VERSION is required}"
VERSION="${VERSION#v}"
TRACK="${VERSION%.*}"
GO_VERSION="${GO_VERSION:?GO_VERSION is required}"
SRC="${SRC:-registry.odigos.io}"
DEST="${DEST:-gcr.io/odigos-public/odigos-staging/final}"
NPMRC="${NPMRC:-$HOME/.npmrc}"
SERVICE_NAME="services/odigos.endpoints.odigos-public.cloud.goog"
HERE="$(cd "$(dirname "$0")" && pwd)"

[ "$(git -C "$CHECKOUT" rev-parse HEAD)" = "$(git -C "$CHECKOUT" rev-parse "v${VERSION}^{commit}")" ] \
  || { echo "ERROR: $CHECKOUT is not at v${VERSION}" >&2; exit 1; }

ctx="$(mktemp -d)"
trap 'rm -rf "$ctx"' EXIT

ODIGLET_SRC=/go/src/github.com/odigos-io/odigos/odiglet/enterprise

# Prints the component's Dockerfile with the golang builder bumped to GO_VERSION.
bumped_dockerfile() {
  sed -E "s#golang:1\.[0-9]+\.[0-9]+(-[a-z]+)?#golang:${GO_VERSION}\1#g" "$CHECKOUT/$1"
}

# Builds and publishes one component. $1 = component, stdin = Dockerfile whose `patched` stage is the official
# image with the new binaries; remaining args go to docker buildx build.
publish() {
  local c="$1" src="${SRC}/odigos-${1}:v${VERSION}"
  shift
  {
    cat
    crane config --platform linux/amd64 "$src" | python3 "$HERE/image-config-stage.py"
  } > "$ctx/Dockerfile.$c"
  docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --provenance=false --sbom=false \
    --build-arg MP_BASE="${src}@$(crane digest "$src")" \
    --annotation "index:com.googleapis.cloudmarketplace.product.service.name=${SERVICE_NAME}" \
    --tag "${DEST}/odigos-${c}:v${VERSION}" \
    --tag "${DEST}/odigos-${c}:${VERSION}" \
    --tag "${DEST}/odigos-${c}:${TRACK}" \
    --output type=image,push=true,oci-mediatypes=true \
    -f "$ctx/Dockerfile.$c" "$@"
  crane manifest "${DEST}/odigos-${c}:${VERSION}" | grep -q 'cloudmarketplace.product.service.name' \
    || { echo "ERROR: ${DEST}/odigos-${c}:${VERSION} is missing the Marketplace annotation" >&2; exit 1; }
}

# Appends the `patched` stage to a component Dockerfile. Args: "<builder path>:<image path>"...
# MP_BASE is declared before the first FROM (after a `# syntax=` line, which must stay first).
with_patched_stage() {
  awk 'NR == 1 && /^# syntax=/ { print; print "ARG MP_BASE"; next } NR == 1 { print "ARG MP_BASE" } { print }'
  printf '\nFROM ${MP_BASE} AS patched\n'
  local m
  for m in "$@"; do printf 'COPY --from=builder %s %s\n' "${m%%:*}" "${m#*:}"; done
}

for c in "$@"; do
  echo "==> ${c} (go${GO_VERSION})"
  case "$c" in
    enterprise-odiglet)
      : "${ODIGLET_BASE_IMAGE:?ODIGLET_BASE_IMAGE is required for enterprise-odiglet}"
      : "${GITHUB_TOKEN:?GITHUB_TOKEN is required for enterprise-odiglet}"
      # The builder normally cross-compiles from the build platform. The official deviceplugin and CSI
      # driver are cgo binaries (built natively per arch), so build per target platform instead.
      bumped_dockerfile odiglet/enterprise/Dockerfile \
        | sed -E 's#^FROM --platform=\$BUILDPLATFORM \$\{ODIGLET_BASE_IMAGE\} AS builder#FROM ${ODIGLET_BASE_IMAGE} AS builder#' \
        | with_patched_stage \
            "${ODIGLET_SRC}/odiglet:/root/odiglet" \
            "${ODIGLET_SRC}/odigos-csi-driver:/root/odigos-csi-driver" \
            "${ODIGLET_SRC}/deviceplugin:/root/deviceplugin" \
            "/tmp/grpc_health_probe:/root/grpc_health_probe" \
        | publish "$c" \
            --build-arg ODIGLET_BASE_IMAGE="$ODIGLET_BASE_IMAGE" \
            ${GRPC_HEALTH_PROBE_VERSION:+--build-arg GRPC_HEALTH_PROBE_VERSION="$GRPC_HEALTH_PROBE_VERSION"} \
            --build-arg LD_FLAGS="-s -w" \
            --secret id=github_token,env=GITHUB_TOKEN \
            "$CHECKOUT"
      ;;
    enterprise-instrumentor)
      bumped_dockerfile instrumentor/enterprise/Dockerfile \
        | with_patched_stage "/workspace/build/instrumentor:/app" \
        | publish "$c" --build-arg LD_FLAGS="-s -w" "$CHECKOUT"
      ;;
    enterprise-collector)
      bumped_dockerfile collector/Dockerfile \
        | with_patched_stage \
            "/go/src/collector/odigosotelcol/odigosotelcol:/odigosotelcol" \
            "/go/src/collector/cmd/odigosopampsupervisor/odigosopampsupervisor:/odigosopampsupervisor" \
        | publish "$c" "$CHECKOUT"
      ;;
    enterprise-ui)
      [ -s "$NPMRC" ] || { echo "ERROR: enterprise-ui needs NPMRC ($NPMRC) with a read:packages token" >&2; exit 1; }
      bumped_dockerfile ui/Dockerfile \
        | with_patched_stage "/workspace/build/ui:/app" \
        | publish "$c" --build-arg SERVICE_NAME=ui --build-arg LD_FLAGS="-s -w" \
            --secret id=npmrc,src="$NPMRC" "$CHECKOUT"
      ;;
    cli)
      # The release builds the CLI image with ko (cli/.ko.yaml). Rebuild it the same way: same flags and version
      # stamps (read the date from the official binary with `strings`), but with GO_VERSION. The official image's
      # binary embeds no Helm chart (only cli/pkg/helm/embedded/.keep), so neither does this one; check
      # `strings <binary> | grep embedded/` on the official image if that changes.
      : "${CLI_DATE:?CLI_DATE is required for cli (OdigosDate of the official binary)}"
      for arch in amd64 arm64; do
        (cd "$CHECKOUT/cli" && GOTOOLCHAIN="go${GO_VERSION}" GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
          go build -trimpath -tags=embed_manifests -o "$ctx/cli-bin/cli-$arch" -ldflags "
            -X github.com/odigos-io/odigos/cli/cmd.OdigosVersion=v${VERSION}
            -X github.com/odigos-io/odigos/cli/pkg/helm.OdigosChartVersion=v${VERSION}
            -X github.com/odigos-io/odigos/cli/cmd.OdigosCommit=${CLI_COMMIT:-}
            -X github.com/odigos-io/odigos/cli/cmd.OdigosDate=${CLI_DATE}" .)
      done
      printf 'ARG MP_BASE\nFROM ${MP_BASE} AS patched\nARG TARGETARCH\nCOPY --chmod=555 cli-${TARGETARCH} /ko-app/cli\n' \
        | publish "$c" "$ctx/cli-bin"
      ;;
    *)
      echo "ERROR: unknown component $c" >&2
      exit 1
      ;;
  esac
done

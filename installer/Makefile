# Variables
ORG ?= gcr.io/odigos-public
IMAGE_NAME = odigos
TAG ?= 1.40.0
# Chart version from oci://registry.odigos.io/charts/odigos (must exist for `helm pull`); keep in sync with TAG
CHART_VERSION ?= $(TAG)
# Marketplace requires every image to also carry the minor-version track tag (e.g. 1.40 for 1.40.0)
TRACK = $(basename $(TAG))
PLATFORMS = linux/amd64,linux/arm64

# Full image reference
IMAGE = $(ORG)/$(IMAGE_NAME):$(TAG)
TRACK_IMAGE = $(ORG)/$(IMAGE_NAME):$(TRACK)

.PHONY: vendor-helm-chart
vendor-helm-chart:
	helm pull oci://registry.odigos.io/charts/odigos --version $(CHART_VERSION) --destination .
	mv odigos-$(CHART_VERSION).tgz odigos-chart.tgz && echo "Wrote odigos-chart.tgz for local go build"

.PHONY: push-image
push-image:
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg CHART_VERSION=$(CHART_VERSION) \
		--tag $(IMAGE) \
		--tag $(TRACK_IMAGE) \
		--push \
		--annotation "index:com.googleapis.cloudmarketplace.product.service.name=services/odigos.endpoints.odigos-public.cloud.goog" \
		.

.PHONY: build-image
build-image:
	docker buildx build \
		--platform linux/amd64 \
		--build-arg CHART_VERSION=$(CHART_VERSION) \
		--tag $(IMAGE) \
		--load \
		.

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  vendor-helm-chart - Download odigos-chart.tgz for local go build / tests"
	@echo "  push-image        - Build multi-arch image and push to registry"
	@echo "  build-image       - Build multi-arch image locally (without push)"
	@echo ""
	@echo "Variables:"
	@echo "  ORG            - Registry organization (default: gcr.io/odigos)"
	@echo "  TAG            - Image tag (default: latest)"
	@echo "  CHART_VERSION  - Odigos Helm chart version to embed (default: same as TAG)"
	@echo "  PLATFORMS      - Target platforms (default: linux/amd64,linux/arm64)"
	@echo ""
	@echo "Example usage:"
	@echo "  make push-image"
	@echo "  make push-image ORG=gcr.io/myorg TAG=v1.0.0"


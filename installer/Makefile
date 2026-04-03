# Variables
ORG ?= gcr.io/odigos-public
IMAGE_NAME = odigos
TAG ?= 1.23.0
# Chart version from https://odigos-io.github.io/odigos/ (must exist for `helm pull`); keep in sync with TAG
CHART_VERSION ?= $(TAG)
PLATFORMS = linux/amd64,linux/arm64

# Full image reference
IMAGE = $(ORG)/$(IMAGE_NAME):$(TAG)

.PHONY: vendor-helm-chart
vendor-helm-chart:
	helm repo add odigos https://odigos-io.github.io/odigos/ 2>/dev/null || true
	helm repo update odigos
	helm pull odigos/odigos --version $(CHART_VERSION) --destination .
	@f=$$(ls helm-chart-odigos-*.tgz 2>/dev/null | head -1) && mv "$$f" odigos-chart.tgz && echo "Wrote odigos-chart.tgz for local go build"

.PHONY: push-image
push-image:
	docker buildx build \
		--platform $(PLATFORMS) \
		--build-arg CHART_VERSION=$(CHART_VERSION) \
		--tag $(IMAGE) \
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


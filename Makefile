ORG ?= gcr.io/odigos-public
IMAGE_NAME = odigos/deployer
TAG ?= 1.39.0
# Marketplace requires every image to also carry the minor-version track tag (e.g. 1.39 for 1.39.0)
TRACK = $(basename $(TAG))
PLATFORMS = linux/amd64

# Full image reference
IMAGE = $(ORG)/$(IMAGE_NAME):$(TAG)
TRACK_IMAGE = $(ORG)/$(IMAGE_NAME):$(TRACK)

# The Marketplace schema extractor needs the deployer as a single-platform image manifest, not an
# image index ("Image not found"), so skip the provenance/SBOM attestations that would wrap it in one.
.PHONY: push-image
push-image:
	docker buildx build \
		--platform $(PLATFORMS) \
		--tag $(IMAGE) \
		--tag $(TRACK_IMAGE) \
		--provenance=false \
		--sbom=false \
		--output type=image,push=true,oci-mediatypes=false \
		--annotation "manifest:com.googleapis.cloudmarketplace.product.service.name=services/odigos.endpoints.odigos-public.cloud.goog" \
		.

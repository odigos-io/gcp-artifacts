# gcp-artifacts

GCP Marketplace Kubernetes app package for Odigos (product `odigos.endpoints.odigos-public.cloud.goog`,
images in `gcr.io/odigos-public/odigos`, GCP project `odigos-public`).

To publish a new Odigos version to the Marketplace, follow [RELEASING.md](RELEASING.md) step by step.

- `schema.yaml` + `manifest/*.template` → deployer image (root `Dockerfile`, amd64 only).
- `installer/` → main image: Go program that `helm install`s the embedded Odigos chart with Marketplace image overrides.
- `hack/publish-component-images.sh` → copies official Odigos component images into the Marketplace registry with the required annotation and tags.
- The local machine is an Apple Silicon Mac; Marketplace needs amd64. `gcloud` lives in `~/google-cloud-sdk/bin`
  (not on PATH by default), and the default gcloud project is not `odigos-public`, so always pass `--project odigos-public`.

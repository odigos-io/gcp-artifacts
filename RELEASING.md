# Releasing a new Odigos version to GCP Marketplace

This is the runbook for publishing a new Odigos version as a new release of the
Kubernetes app product `odigos.endpoints.odigos-public.cloud.goog`. It is written to
be followed step by step by a person or an agent.

References:
- [Create the app package](https://docs.cloud.google.com/marketplace/docs/partners/kubernetes/create-app-package)
- [Submit the app](https://docs.cloud.google.com/marketplace/docs/partners/kubernetes/submitting)
- [Maintain the product](https://docs.cloud.google.com/marketplace/docs/partners/kubernetes/maintaining-product)
- [mpdev reference](https://github.com/GoogleCloudPlatform/marketplace-k8s-app-tools/blob/master/docs/mpdev-references.md)

## How the package fits together

| Image | Built from | Arch |
|---|---|---|
| `gcr.io/odigos-public/odigos/deployer:<ver>` | root `Dockerfile` (`deployer_envsubst/onbuild` + `schema.yaml` + `manifest/`) | amd64 |
| `gcr.io/odigos-public/odigos:<ver>` (main image, `imageFull`) | `installer/` Go program with the Odigos Helm chart embedded | amd64+arm64 |
| `gcr.io/odigos-public/odigos/odigos-<component>:<ver>` | official Odigos release images, re-indexed with the Marketplace annotation (`hack/publish-component-images.sh`) | amd64+arm64 |

At install time Marketplace runs the deployer. The deployer applies `manifest/*.template`, which
creates the `Application` and the installer Deployment (plus the `ubbagent` billing sidecar).
The installer runs `helm install`/`upgrade` with the embedded chart, overriding `images.*` with
the Marketplace image refs passed via env vars (see `installer/helm_install.go`). It then stays
up to report node counts and to run `helm uninstall` when the Application is deleted.

Marketplace rules that bite:
- **Every** image (deployer, main image, every component in `schema.yaml`) needs both the
  full version tag (`1.39.0`) and the minor-version track tag (`1.39`). Component images also
  get `v1.39.0`.
- Every image must carry the annotation
  `com.googleapis.cloudmarketplace.product.service.name=services/odigos.endpoints.odigos-public.cloud.goog`
  (on the index for multi-arch images, on the manifest for the deployer).
- The **deployer must be a single-platform image manifest, not an image index**. An index (e.g. the
  default buildx output with a provenance attestation) makes Producer Portal validation fail with
  `EXTRACT_K8S_APP_SCHEMA ... Failed to extract schema from deployer ...@sha256:... Image not found`.
  The root `Makefile` handles this (`--provenance=false --sbom=false`, Docker v2 media types,
  `manifest:` annotation). App and component images may be multi-arch indexes.
- `schema.yaml` `publishedVersion` must equal the deployer tag, and the Application
  `spec.descriptor.version` must match it.
- Marketplace clusters are x86, so amd64 is required for everything. The deployer is amd64-only.

## Prerequisites (local Mac)

- `gcloud` (at `~/google-cloud-sdk/bin`; put it on `PATH` so `docker-credential-gcloud` resolves),
  `docker` with buildx, `helm`, `crane`, `kubectl`, `mpdev` (`/opt/homebrew/bin/mpdev`).
- `gcloud auth login` (the token expires; non-interactive runs fail with "Reauthentication failed").
- `gcloud auth configure-docker` (`gcr.io` → `gcloud` credHelper in `~/.docker/config.json`).
- The default gcloud project is `odigos-cloud`. Pass `--project odigos-public` explicitly rather
  than changing it.

## Steps

Set `VERSION=1.39.0` (no `v`) and `TRACK=1.39` below.

### 1. Pick the version and check the chart

```bash
gh release list -R odigos-io/odigos -L 5          # latest stable release
helm repo update odigos && helm search repo odigos/odigos --versions | head
```

The chart version must exist (`odigos/odigos` `$VERSION`).

Check that the chart still uses the same image components the installer overrides. Diff
`helm show values` between the old and new versions, then render with the marketplace overrides
and compare the image list:

```bash
helm template x odigos/odigos --version $VERSION -n ns --set image.tag=v$VERSION \
  --set images.autoscaler=MP/autoscaler --set images.scheduler=MP/scheduler \
  --set images.collector=MP/collector --set images.ui=MP/ui \
  --set images.instrumentor=MP/instrumentor --set images.odiglet=MP/odiglet \
  --set images.agents=MP/agents | grep -E '^\s+(image|value): ' | sort | uniq -c
```

Every Odigos image should resolve to an `MP/...` ref. Repeat with `--set onPremToken=x` and the
`enterprise-*` keys. If the chart adds, renames or removes a component (the values file already
says autoscaler/scheduler are merged into instrumentor; they are still deployed as of 1.39.0),
update all of these together:
- `schema.yaml` `x-google-marketplace.images`
- the env vars in `manifest/manifests.yaml.template`
- `marketplaceImageOverrides` in `installer/helm_install.go`
- `COMPONENTS` in `hack/publish-component-images.sh`

Known gap: `odigos-cli` (cleanup hook) and, for enterprise installs, `enterprise-collector` /
`enterprise-ui` are not overridden and pull from Odigos registries directly.

### 2. Bump the version strings

Replace the old version in: `Makefile` (`TAG`), `installer/Makefile` (`TAG`),
`installer/Dockerfile` (`CHART_VERSION`), `schema.yaml` (`publishedVersion`, and update
`releaseNote` if useful), `manifest/application.yaml.template` (`descriptor.version`),
`manifest/manifests.yaml.template` (`ODIGOS_VERSION`).

```bash
grep -rn "<old-version>" --exclude-dir=.git .
```

Build check:

```bash
make -C installer vendor-helm-chart
(cd installer && GOWORK=off go build ./... && GOWORK=off go vet ./...)
```

(`GOWORK=off` because the local, git-ignored `go.work` can lag the installer's Go version.)

### 3. Publish component images

```bash
PATH=$HOME/google-cloud-sdk/bin:$PATH hack/publish-component-images.sh $VERSION
```

This re-indexes `registry.odigos.io/odigos-<c>:v$VERSION` into
`gcr.io/odigos-public/odigos/odigos-<c>` with the annotation and tags `v$VERSION`, `$VERSION`, `$TRACK`.
It moves every layer through the local machine and can take 10–20 minutes. Don't run other big
pushes at the same time; parallel pushes caused `failed to authorize: DeadlineExceeded`.

Older releases (≤1.35.3) rebuilt these from source with
`GCP_MARKETPLACE=true ORG=gcr.io/odigos-public/odigos TAG=v$VERSION PUSH_IMAGE=true` on the
`push-images` targets of the Odigos Makefile. That route is still valid but much slower on a Mac.

#### 3b. Vulnerability scan findings

Producer Portal **Save and Validate** also runs a vulnerability scan and rejects the release if any
image contains packages with known fixed CVEs. The report lists image digest, package, affected and
fixed version. Fix the source in Odigos (and cut a release) when possible. To ship a release before
that, patch only the flagged components:

- **Installer** (`odigos` main image): bump the `golang:` images in `installer/Dockerfile` (and
  `go` in `installer/go.mod`), then rerun step 4's installer push.
- **Go stdlib in a component** (e.g. the collector): rebuild that component from the Odigos release tag
  with the newer Go builder image (`git worktree add ... v$VERSION`, edit the component Dockerfile,
  `make push-<component> ORG=gcr.io/odigos-public/odigos-staging TAG=v$VERSION PUSH_IMAGE=true GCP_MARKETPLACE=true`),
  check the binary with `go version`, then re-index it into the final tags as in step 3.
- **Bundled agent files** (PHP vendor packages, the java-ebpf jar, `grpc_health_probe`):
  `hack/patch-component-images.sh` overlays replacements onto the official images, copies the result
  into a single layer (so the replaced files are gone from every layer, not just hidden), restores the
  original image config (`hack/image-config-stage.py`) and publishes it as an annotated OCI index.
  Edit `hack/patch-component-images.Dockerfile` for what needs replacing. Run it with
  `BUILDX_BUILDER=multibuilder DEST=gcr.io/odigos-public/odigos-staging/final` first, check the result
  (Go versions, package versions, unchanged files elsewhere), then promote each of the three tags with
  `crane copy <staging> gcr.io/odigos-public/odigos/odigos-<c>:<tag>`.
  Don't use `crane flatten` here: on these images it writes an OCI index pointing at Docker v2
  manifests, which gcr.io rejects (400). And don't push Docker v2 manifest lists: they can't carry the
  annotation, and `imagetools create` drops it without an error.

1.39.0 findings, for reference: Go stdlib CVE-2026-56857 (needs Go ≥ 1.26.9) in installer, collector
and odiglet's `grpc_health_probe`; `laravel/framework` CVE-2026-114509 in the PHP agent bundle
(fixed by php-community v0.10.0); CVE-2015-1427 reported for `org.elasticsearch:elasticsearch`, a false
positive caused by a bundled instrumentation module whose Maven artifactId was `elasticsearch`.

### 4. Build and push the installer (main image) and the deployer

```bash
export PATH=$HOME/google-cloud-sdk/bin:$PATH
make -C installer push-image   # gcr.io/odigos-public/odigos:$VERSION and :$TRACK (amd64+arm64)
BUILDX_BUILDER=multibuilder make push-image   # gcr.io/odigos-public/odigos/deployer:$VERSION and :$TRACK (amd64)
```

The deployer must be pushed with a `docker-container` buildx builder (`multibuilder` here; create
one with `docker buildx create --name multibuilder --driver docker-container`). With the default
Docker Desktop builder the push fails with `failed to authorize: DeadlineExceeded`, apparently
while BuildKit asks for a token to cross-mount the base layers from `gcr.io/cloud-marketplace-tools`.

If a push only produced `$VERSION`, add the track tag afterwards with
`crane tag gcr.io/odigos-public/odigos/<image>:$VERSION $TRACK`.

Check tags and annotations. The deployer must print the Docker v2 manifest media type and
`index-entries=0`:

```bash
crane manifest gcr.io/odigos-public/odigos/deployer:$VERSION | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["mediaType"], d.get("annotations"), "index-entries=%d" % len(d.get("manifests",[])))'
for i in "" /deployer /odigos-collector; do crane ls gcr.io/odigos-public/odigos$i | grep "$TRACK"; done
```

### 5. Verify on GKE with mpdev

```bash
gcloud container clusters create odigos-mp-verify --project odigos-public --zone us-central1-a \
  --num-nodes 2 --machine-type e2-standard-4
# Use a dedicated kubeconfig so the user's current kubectl context is not switched
export KUBECONFIG=/tmp/mpdev-kubeconfig
gcloud container clusters get-credentials odigos-mp-verify --project odigos-public --zone us-central1-a
kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/marketplace-k8s-app-tools/master/crd/app-crd.yaml

# The Homebrew `mpdev` (/opt/homebrew/bin/mpdev) is Google's newer Go CLI and has no `verify`.
# Use the k8s dev wrapper from Google's image (needs gcloud on PATH for the gcr.io pull):
docker run --rm --platform linux/amd64 gcr.io/cloud-marketplace-tools/k8s/dev cat /scripts/dev > /tmp/mpdev && chmod +x /tmp/mpdev
# It mounts KUBE_CONFIG (default ~/.kube/config) into the container
KUBE_CONFIG=/tmp/mpdev-kubeconfig /tmp/mpdev verify --deployer=gcr.io/odigos-public/odigos/deployer:$VERSION
```

`mpdev verify` installs with the schema defaults (and a fake reporting secret), waits for the
Application to become ready, runs any tester, then uninstalls. It must end with
`VERIFICATION STATUS: PASSED`. Readiness-probe warnings in its "ERROR SUMMARY" from the first
~90s of startup are normal. The mpdev container is amd64 and runs under emulation on Apple Silicon.

Expected state of a healthy install (as of 1.39.0): `odigos` (installer + ubbagent), `odigos-gateway`,
`odigos-instrumentor`, `odigos-ui` Deployments ready, `odiglet` DaemonSet 3/3 on every node.
`odigos-autoscaler` and `odigos-scheduler` exist with `replicas: 0` (compatibility stubs; their
logic moved into the instrumentor). Keep them in `schema.yaml` while the chart still references them.

For a manual look (UI, pods, then deletion and finalizer cleanup):

```bash
kubectl create ns odigos-test
# reportingSecret is required; a dummy one works (ubbagent will crashloop on it, which is expected)
kubectl -n odigos-test create secret generic odigos-reporting \
  --from-literal=reporting-key=$(echo '{}' | base64) --from-literal=consumer-id=project:test
KUBE_CONFIG=/tmp/mpdev-kubeconfig /tmp/mpdev install --deployer=gcr.io/odigos-public/odigos/deployer:$VERSION \
  --parameters='{"name": "odigos", "namespace": "odigos-test", "reportingSecret": "odigos-reporting"}'
kubectl -n odigos-test rollout status ds/odiglet --timeout=300s
kubectl -n odigos-test get pods -o custom-columns='NAME:.metadata.name,READY:.status.containerStatuses[*].ready,IMAGES:.spec.containers[*].image'
kubectl -n odigos-test logs deploy/odigos -c installer
kubectl -n odigos-test delete application odigos   # installer should helm-uninstall and drop its finalizer
```

All Odigos pods should run `gcr.io/odigos-public/odigos/...` images. After the Application is deleted,
the installer logs `Helm uninstall completed` and `removed finalizer`, and no Odigos CRDs, webhooks,
APIServices or cluster RBAC remain. Exceptions: the
`odigos-test:odigos:installerServiceAccount-r0`/`-rb0` ClusterRole/Binding that `mpdev install`
itself created; delete them by hand.

Delete the cluster afterwards:

```bash
gcloud container clusters delete odigos-mp-verify --project odigos-public --zone us-central1-a --quiet
```

### 6. Commit and open a PR

Branch `bump-$VERSION`, open a PR against `odigos-io/gcp-artifacts` `main`.

### 7. Submit in Producer Portal (human step)

1. Open https://console.cloud.google.com/producer-portal?project=odigos-public → Odigos product →
   **Container images** (Edit).
2. **Add release**: pick display tag `$TRACK` (new minor track), add a description, the public
   Git repo (`https://github.com/odigos-io/gcp-artifacts`) and the deploy documentation URL.
   Make it the default release if it should be the default for new customers.
3. **Save and Validate** (Google copies the images and runs its own verification), then **Submit**.
4. After review approval (image review can take up to two weeks), **Publish** from the Overview page.

For a patch release on an existing track (e.g. `1.39.1` on `1.39`), push the new images with the
same track tag, then in **Proposed releases → Edit** re-select the display tag so it maps to the new
digest, and run **Save and Validate → Submit → Publish** again.

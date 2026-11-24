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
  full version tag (`1.40.0`) and the minor-version track tag (`1.40`). Component images also
  get `v1.40.0`.
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
- Since 1.40 `onPremToken` is a required `MASKED_FIELD` in `schema.yaml`. The chart has no
  community tier: odiglet, instrumentor and UI read the `odigos-pro` secret, which the chart only
  creates from the token, and the installer exits with an error when the token is empty.

## Prerequisites (local Mac)

- `gcloud` (at `~/google-cloud-sdk/bin`; put it on `PATH` so `docker-credential-gcloud` resolves),
  `docker` with buildx, `helm`, `crane`, `kubectl`, `mpdev` (`/opt/homebrew/bin/mpdev`).
- `gcloud auth login` (the token expires; non-interactive runs fail with "Reauthentication failed").
- `gcloud auth configure-docker` (`gcr.io` → `gcloud` credHelper in `~/.docker/config.json`).
- The default gcloud project may not be `odigos-public`. Pass `--project odigos-public` explicitly
  rather than changing it.

## Steps

Set `VERSION=1.40.0` (no `v`) and `TRACK=1.40` below.

### 1. Pick the version and check the chart

```bash
gh release list -R odigos-io/odigos -L 5          # latest stable release
helm show chart oci://registry.odigos.io/charts/odigos --version $VERSION
```

The chart version must exist in `oci://registry.odigos.io/charts/odigos`. (The old
`https://odigos-io.github.io/odigos/` repo index stops at 1.39.0; don't use it.)

Check that the chart still uses the same image components the installer overrides. Diff
`helm show values` between the old and new versions, then render with the marketplace overrides
and compare the image list:

```bash
helm template x oci://registry.odigos.io/charts/odigos --version $VERSION -n ns --set image.tag=v$VERSION \
  --set onPremToken=x \
  --set images.autoscaler=MP/autoscaler --set images.scheduler=MP/scheduler --set images.cli=MP/cli \
  --set images.enterprise-instrumentor=MP/enterprise-instrumentor \
  --set images.enterprise-odiglet=MP/enterprise-odiglet --set images.enterprise-agents=MP/enterprise-agents \
  --set images.enterprise-collector=MP/enterprise-collector --set images.enterprise-ui=MP/enterprise-ui \
  | grep -E '^\s+(image|value): ' | sort | uniq -c
```

Every Odigos image should resolve to an `MP/...` ref. Since 1.40.0 the chart has no community tier:
it always deploys the `enterprise-*` images, and the community images (`odigos-collector`,
`odigos-instrumentor`, `odigos-odiglet`, `odigos-ui`, `odigos-agents`) are no longer published.
Optional components that `schema.yaml` gives users no way to enable (`enterprise-insights`, `cache`,
`victoria-metrics`, `enterprise-central-proxy`, ClickHouse) stay unmapped.

If the chart adds, renames or removes a component (the values file already says
autoscaler/scheduler are merged into instrumentor; they are still deployed as of 1.40.0),
update all of these together:
- `schema.yaml` `x-google-marketplace.images`
- the env vars in `manifest/manifests.yaml.template`
- `marketplaceImageOverrides` in `installer/helm_install.go`
- `COMPONENTS` in `hack/publish-component-images.sh`

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
fixed version. The findings have to be fixed in the images under the tags you already pushed; then run
**Save and Validate** again. Work through them in this order:

1. **Triage.** For each finding, find the file in the image and what produced it (`crane export` the
   image, `go version -m <binary>` for Go binaries). Sort each into one of the paths below.
2. **Fix upstream.** Check whether Odigos `main` already has the fix (builder `golang:` images,
   `odiglet/base.Dockerfile`, pinned tool versions such as `GRPC_HEALTH_PROBE_VERSION`). Open a PR
   against `odigos-io/odigos` `main` for anything it doesn't, referencing the release ticket, so the next
   release passes the scan as published.
3. **Patch this release** with the paths below, building into
   `gcr.io/odigos-public/odigos-staging/...` first.
4. **Check** each patched image against the official one (see "Checking a patched image").
5. **Re-verify** with `mpdev verify` from staging if the deployer, the installer or a component's
   binaries changed (see "Re-verifying from staging").
6. **Promote** to prod, check tags and annotations as in step 4, and run **Save and Validate** again.

Patch paths:

- **Installer** (`odigos` main image): bump the `golang:` images in `installer/Dockerfile` (and
  `go` in `installer/go.mod`); bump flagged modules with
  `(cd installer && GOWORK=off go get <module>@<fixed> && GOWORK=off go mod tidy)`. Rebuild with step 4's
  installer push (to staging first: `make -C installer push-image ORG=gcr.io/odigos-public/odigos-staging/<dir>`).
- **Deployer**: Google's `deployer_envsubst/onbuild` base ships kubectl, and its Go version can lag the
  scan even in the latest base. The root `Dockerfile` rebuilds the same kubectl version from source with a
  newer `golang:` image and drops it over the original; bump `golang:` / `KUBECTL_VERSION` there (check
  `go version` on `/opt/kubectl/*/kubectl` in the base first), then rerun step 4's deployer push. The
  verification token from step 4 can be reused while it is valid.
- **Go stdlib in a component** (Odigos binaries built with an older Go):
  `hack/rebuild-component-binaries.sh <odigos checkout at v$VERSION> <component>...` rebuilds only the
  component's Go binaries with its own Dockerfile and `GO_VERSION` as the builder, swaps them into the
  official image, flattens it into one layer with the original config, and publishes an annotated OCI
  index (all three tags) to `DEST`. Components: `enterprise-odiglet`, `enterprise-instrumentor`,
  `enterprise-collector`, `enterprise-ui`, `cli`. Use a clean worktree of the release tag
  (`git worktree add --detach <dir> v$VERSION`); the script bumps the Go version itself. The script
  header lists the environment it needs:
  - `enterprise-odiglet` builds on an odiglet base image with the new Go: build `odiglet/base.Dockerfile`
    from the checkout with its `golang:` bumped (`--platform linux/amd64,linux/arm64`, push to
    `odigos-staging/odiglet-base:<tag>`) and pass it as `ODIGLET_BASE_IMAGE`. It also needs a
    `GITHUB_TOKEN` that can read the private instrumentation modules (`GITHUB_TOKEN=$(gh auth token)`),
    and takes `GRPC_HEALTH_PROBE_VERSION` to fix `grpc_health_probe` in the same build. Its deviceplugin
    and CSI driver are cgo binaries, so the builder runs per target platform (amd64 under emulation on a
    Mac) and the build takes much longer than the others.
  - `enterprise-ui` needs `NPMRC`: an `.npmrc` with a GitHub Packages token for the UI kit. With the gh
    CLI: `gh auth refresh -h github.com -s read:packages` once, then write
    `//npm.pkg.github.com/:_authToken=<gh auth token>` to a mode-600 file outside the repo and delete
    it after the build.
  - `cli` needs `CLI_DATE`, the official binary's build date: `crane export` the official image and run
    `strings ko-app/cli | grep -E '^20[0-9]{2}-[0-9]{2}-[0-9]{2}T'`. The official CLI image embeds no
    Helm chart, and neither does the rebuild.

  Run it with `BUILDX_BUILDER=multibuilder DEST=gcr.io/odigos-public/odigos-staging/final`. Run one
  build at a time: `go mod download` inside the builds intermittently fails with
  `stream error: ... INTERNAL_ERROR; received from peer` from proxy.golang.org, more often when several
  builds download at once. Rerun the script on that error; BuildKit's module cache keeps what was
  already downloaded.
- **Bundled agent files** (PHP vendor packages, the java-ebpf jar, `grpc_health_probe` alone):
  `hack/patch-component-images.sh` overlays replacements onto the official images, copies the result
  into a single layer (so the replaced files are gone from every layer, not just hidden), restores the
  original image config (`hack/image-config-stage.py`) and publishes it as an annotated OCI index.
  Edit `hack/patch-component-images.Dockerfile` for what needs replacing. Run it with
  `BUILDX_BUILDER=multibuilder DEST=gcr.io/odigos-public/odigos-staging/final` first.

Don't use `crane flatten` for any of this: on these images it writes an OCI index pointing at Docker v2
manifests, which gcr.io rejects (400). And don't push Docker v2 manifest lists: they can't carry the
annotation, and `imagetools create` drops it without an error.

**Checking a patched image.** Export the patched and the official image for each architecture
(`crane export --platform linux/<arch> <ref> - > <file>.tar`) and check:
- `go version -m` on every replaced binary: the new Go version, and the same build settings as the
  official binary (`-ldflags`, `CGO_ENABLED`, `-tags`).
- Everything else is unchanged: compare every tar entry except the replaced files (content hash, mode,
  owner, and the `SCHILY.xattr.*` PAX headers, which hold file capabilities). odiglet and the collector
  binary carry file capabilities, and they must match the official image.
- The image config (`crane config`: entrypoint, user, env, working directory) matches the official one.
- The index carries the annotation and the three tags share one digest.

**Re-verifying from staging.** `mpdev verify` derives every image from the deployer's path, so put a
complete copy of the package under one staging prefix: the installer at `<prefix>/odigos:$VERSION`,
the deployer at `<prefix>/odigos/deployer:$VERSION`, and every component in `schema.yaml` at
`<prefix>/odigos/odigos-<c>:$VERSION`. Copy the unchanged components from prod with `crane copy`, push
the rebuilt installer and deployer there with `ORG=<prefix>`, then run step 5 with
`--deployer=<prefix>/odigos/deployer:$VERSION`. The log should show every image pulled from `<prefix>`.

**Promoting.** `crane copy <staging>:<tag> gcr.io/odigos-public/odigos/<repo>:<tag>` for each tag
(`v$VERSION`, `$VERSION` and `$TRACK` for components, `$VERSION` and `$TRACK` for the installer and
deployer). This overwrites the tags Producer Portal already points at, so the next **Save and Validate**
scans the patched images.

1.39.0 findings, for reference: Go stdlib CVE-2026-56857 (needs Go ≥ 1.26.9) in installer, collector
and odiglet's `grpc_health_probe`; `laravel/framework` CVE-2026-114509 in the PHP agent bundle
(fixed by php-community v0.10.0); CVE-2015-1427 reported for `org.elasticsearch:elasticsearch`, a false
positive caused by a bundled instrumentation module whose Maven artifactId was `elasticsearch`.

1.40.0 findings, for reference: Go stdlib CVE-2026-97040 (needs Go 1.26.11 / 1.27.4) in the installer,
the deployer's kubectl and every Odigos binary (odiglet, deviceplugin, CSI driver, instrumentor,
collector, supervisor, UI, CLI), plus `grpc_health_probe` v0.4.59 (fixed by v0.4.63); `golang.org/x/crypto`
CVEs in the installer (fixed by v0.52.0). Fixed with all three paths above (installer, deployer kubectl,
component binary rebuilds). Odigos `main` was already on Go 1.27.4; only the `grpc_health_probe` bump
went upstream.

### 4. Build and push the installer (main image) and the deployer

The deployer needs a **verification token**. Google verifies with schema defaults only, and
`onPremToken` is required with no default, so the deployer carries a test-only default in
`/data-test/schema.yaml`. Only the deployer's test mode reads it; customer installs ignore it and must
enter their own token. Anyone can read it from the public deployer image, so it must be a dedicated
token that expires soon after review:

1. In `odigos-io/odigos`, run the **Generate On-Prem Token** workflow with `jwt_audience`
   `gcp-marketplace-verification-$VERSION`, `token_expiry_days` `30` (covers Save and Validate plus up
   to two weeks of review), no profiles, and `notify` off.
2. Download the `token.txt` artifact within a day (it is deleted after that) to a file outside the repo,
   e.g. `~/mp-verify-token.txt`. Never commit it; `data-test/` is gitignored.
3. Submit in Producer Portal (step 7) well before it expires. Each new deployer version needs a fresh
   token; a re-validation after expiry fails.

```bash
export PATH=$HOME/google-cloud-sdk/bin:$PATH
make -C installer push-image   # gcr.io/odigos-public/odigos:$VERSION and :$TRACK (amd64+arm64)
BUILDX_BUILDER=multibuilder make push-image VERIFY_TOKEN_FILE=~/mp-verify-token.txt   # gcr.io/odigos-public/odigos/deployer:$VERSION and :$TRACK (amd64)
rm -r data-test
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
# It mounts KUBE_CONFIG (default ~/.kube/config) into the container.
# Like Google's verification, it takes onPremToken from the deployer's /data-test overlay (step 4).
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
# mpdev install is not test mode, so it ignores /data-test: pass a token like a customer would.
# mpdev echoes the parameters, so mask the token in its output.
TOKEN=$(tr -d '[:space:]' < ~/mp-verify-token.txt)
KUBE_CONFIG=/tmp/mpdev-kubeconfig /tmp/mpdev install --deployer=gcr.io/odigos-public/odigos/deployer:$VERSION \
  --parameters="{\"name\": \"odigos\", \"namespace\": \"odigos-test\", \"reportingSecret\": \"odigos-reporting\", \"onPremToken\": \"$TOKEN\"}" \
  2>&1 | sed "s/$TOKEN/***/g"
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

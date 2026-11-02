# Patches an official Odigos component image for the Marketplace vulnerability scan without rebuilding it
# from source. Driven by hack/patch-component-images.sh, which appends a final stage that copies the
# `patched` filesystem into a single layer (so the replaced files do not survive in a lower layer) and
# restores the original image config.
ARG BASE
ARG PATCH_GRPC=no
ARG PATCH_JAVA=no

FROM busybox:1.36.1-musl AS busybox
FROM public.ecr.aws/odigos/agents/php-community:v0.10.0@sha256:84462b470d6f552fd37d9cf2552b521b74cf76df591ed57ae4348d6318faeec6 AS php

# php-community v0.10.0: laravel/framework 12.69.3 (CVE-2026-114509)
FROM ${BASE} AS patched-php
USER root
RUN --mount=type=bind,from=busybox,source=/bin/busybox,target=/busybox ["/busybox", "rm", "-rf", "/instrumentations/php"]
COPY --from=php /instrumentations/php /instrumentations/php

# grpc_health_probe v0.4.61 is built with Go 1.27 (v0.4.55 has Go 1.26.6 stdlib, CVE-2026-56857)
FROM patched-php AS grpc-yes
ARG TARGETARCH
ARG GRPC_HEALTH_PROBE_VERSION=v0.4.61
ADD --chmod=755 https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/${GRPC_HEALTH_PROBE_VERSION}/grpc_health_probe-linux-${TARGETARCH} /root/grpc_health_probe
FROM patched-php AS grpc-no

# dtrace-injector.jar rebuilt so no bundled module is published as Maven artifactId "elasticsearch"
# (the scanner matched it to org.elasticsearch:elasticsearch, CVE-2015-1427)
FROM grpc-${PATCH_GRPC} AS java-yes
COPY dtrace-injector.jar /instrumentations/java-ebpf/dtrace-injector.jar
FROM grpc-${PATCH_GRPC} AS java-no

FROM java-${PATCH_JAVA} AS patched

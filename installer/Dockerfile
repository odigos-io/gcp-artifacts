FROM golang:1.26.1 AS chart

RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash \
    && rm -rf /var/lib/apt/lists/*

# Must match a chart version published at https://odigos-io.github.io/odigos/
ARG CHART_VERSION=1.23.0
WORKDIR /out
RUN helm repo add odigos https://odigos-io.github.io/odigos/ \
    && helm repo update odigos \
    && helm pull odigos/odigos --version "${CHART_VERSION}" --destination /out \
    && f="$(ls /out/*.tgz | head -1)" \
    && mv "$f" /out/odigos-chart.tgz

FROM --platform=$BUILDPLATFORM golang:1.26.1 AS builder

ARG TARGETPLATFORM
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY --from=chart /out/odigos-chart.tgz ./odigos-chart.tgz

COPY go.mod go.sum ./
RUN go mod download

COPY main.go helm_install.go helm_actions.go application_finalizer.go ./

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o installer .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /app/installer /installer

ENTRYPOINT ["/installer"]

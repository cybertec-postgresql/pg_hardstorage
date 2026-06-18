# pg_hardstorage production image.
#
# Distroless final stage: no shell, no package manager, no SUID
# binaries. The :nonroot variant runs as uid/gid 65532. We rebuild
# from source on every CI run so the image is fully reproducible
# from VERSION/COMMIT/BUILD_DATE arguments.
#
# Build:
#   docker build \
#     --build-arg VERSION=v0.1.1 \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) \
#     --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
#     -f deploy/docker/Dockerfile .

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION} AS builder

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# Cache the module download separately from the source tree so an
# unchanged go.sum produces a cache hit even when sources churn.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

RUN go build \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w \
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Version=${VERSION} \
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Commit=${COMMIT} \
        -X github.com/cybertec-postgresql/pg_hardstorage/internal/version.Date=${BUILD_DATE}" \
      -o /out/pg_hardstorage \
      ./cmd/pg_hardstorage

FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="pg_hardstorage" \
      org.opencontainers.image.description="PostgreSQL backup, done right." \
      org.opencontainers.image.source="https://github.com/cybertec-postgresql/pg_hardstorage" \
      org.opencontainers.image.url="https://github.com/cybertec-postgresql/pg_hardstorage" \
      org.opencontainers.image.vendor="CYBERTEC PostgreSQL International GmbH" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=builder /out/pg_hardstorage /usr/bin/pg_hardstorage

# State and config volumes. The image ships empty; operators mount
# their config and a persistent volume for state.
VOLUME ["/var/lib/pg_hardstorage", "/etc/pg_hardstorage"]

USER nonroot:nonroot

ENTRYPOINT ["/usr/bin/pg_hardstorage"]

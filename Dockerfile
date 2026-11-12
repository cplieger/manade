# check=error=true
FROM golang:1.27-alpine@sha256:7d5cbf6833f7331dafd25a2e8b9673477f559759ff8ed4ca8efabe6795ad08db AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# The release passes its tag; a stable tag makes the pool run the stable
# runner images, any other value their dev tags.
ARG BUILD_VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION}" -o /manade ./cmd/manade
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md ./
COPY scripts/collect-licenses.sh scripts/
RUN --mount=type=cache,target=/go/pkg/mod \
    sh scripts/collect-licenses.sh --name manade ./cmd/manade

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

COPY --chmod=755 --from=builder /manade /manade
COPY --from=builder /out/usr/share/licenses /usr/share/licenses
# The pool reaches the Docker socket through the socket's group, which the
# deployment adds; it never needs root.
USER 65532:65532
# Boot pulls the runner images and can wait out a crashed predecessor's
# session claim, so the start period is ManadeBootStalled's 30-minute bound.
# A passing check ends it early, so a booted pool is judged at once
# (https://docs.docker.com/reference/dockerfile/#healthcheck).
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=30m \
    CMD ["/manade", "health"]
ENTRYPOINT ["/manade"]

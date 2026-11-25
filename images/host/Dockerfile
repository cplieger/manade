# check=error=true
FROM golang:1.27-alpine@sha256:7d5cbf6833f7331dafd25a2e8b9673477f559759ff8ed4ca8efabe6795ad08db AS builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# The release passes its tag; the host follows the job images' moving tags of
# that tag's major and channel, a CI or local build's those of major 1, dev.
ARG BUILD_VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${BUILD_VERSION}" -o /manade ./cmd/manade
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md ./
COPY scripts/collect-licenses.sh scripts/
RUN --mount=type=cache,target=/go/pkg/mod \
    sh scripts/collect-licenses.sh --name manade ./cmd/manade

# A new Docker major here is a reviewed change, run end to end on the
# platform floor (kernel 4.4, cgroup v1) before it merges: Docker 29 warns
# that its cgroup v1 support ends by May 2029
# (https://github.com/moby/moby/issues/51111).
FROM docker:29-dind@sha256:fbbc96e0af32cdc190aff92ba97a1281a115351a723afe21af78879cc55126b0
ARG PKG_REFRESH=static
# The image ships both iptables variants and the legacy symlink farm the
# supervisor puts first in PATH on a kernel without nf_tables.
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apk add --no-cache ipset nftables
COPY --chmod=755 --from=builder /manade /manade
COPY --from=builder /out/usr/share/licenses /usr/share/licenses
LABEL manade.runner-image=host
# docker-init (tini) is PID 1: it forwards SIGTERM to the supervisor and
# reaps the orphans a Go process does not, such as a containerd shim whose
# parent has gone.
# The marker goes fresh once the host runs a section with its rules loaded
# and its daemon answering, which on a slow machine's first start takes over
# a minute; a host still waiting for its first section reads unhealthy.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=3m \
    CMD ["/manade", "host", "health"]
ENTRYPOINT ["docker-init", "--", "/manade", "host"]

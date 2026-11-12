# check=error=true
FROM ghcr.io/cplieger/manade-runner:base-dev
USER root
ARG PKG_REFRESH=static
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# hadolint ignore=DL3008
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libssl-dev \
    pkg-config \
    zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
LABEL manade.runner-image=build
USER runner

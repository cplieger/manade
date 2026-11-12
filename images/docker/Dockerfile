# check=error=true
FROM ghcr.io/cplieger/manade-runner:build-dev
USER root
ARG PKG_REFRESH=static
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# The prerequisites Docker lists for its static binaries that the base lacks
# (https://docs.docker.com/engine/install/binaries/); the engine itself is
# already in GitHub's image. No VOLUME: the pool removes containers without
# their volumes, so an anonymous one would outlive every job.
# hadolint ignore=DL3008
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apt-get update && apt-get install -y --no-install-recommends \
    iptables \
    procps \
    && rm -rf /var/lib/apt/lists/*
COPY --chmod=755 images/docker/manade-dockerd-run.sh /usr/local/bin/manade-dockerd-run
LABEL manade.runner-image=docker
USER runner

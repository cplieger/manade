# check=error=true
FROM ghcr.io/cplieger/manade-runner:build-dev
USER root
# renovate: datasource=node-version depName=node versioning=node
ARG NODE_VERSION=26.13.1
# renovate: datasource=npm depName=playwright
ARG PLAYWRIGHT_VERSION=1.65.0
ARG PKG_REFRESH=static
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# Playwright's install-deps is the one list of the browsers' system libraries,
# fonts and xvfb for this Ubuntu release, and it runs apt itself. Node is
# needed only to run it, so it leaves with the layer.
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && case "$(uname -m)" in x86_64) arch=x64 ;; aarch64) arch=arm64 ;; *) exit 1 ;; esac \
    && tarball="node-v${NODE_VERSION}-linux-${arch}.tar.xz" \
    && mkdir -p /tmp/node \
    && curl -fsSL --retry 3 -o "/tmp/node/${tarball}" "https://nodejs.org/dist/v${NODE_VERSION}/${tarball}" \
    && sum=$(curl -fsSL --retry 3 "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" | awk -v f="${tarball}" '$2 == f {print $1}') \
    && [ -n "${sum}" ] && printf '%s  /tmp/node/%s\n' "${sum}" "${tarball}" | sha256sum -c --quiet \
    && tar -xJf "/tmp/node/${tarball}" -C /tmp/node --strip-components=1 \
    && PATH="/tmp/node/bin:${PATH}" npx -y "playwright@${PLAYWRIGHT_VERSION}" install-deps chromium firefox webkit \
    && rm -rf /tmp/node /root/.npm /var/lib/apt/lists/*
# The browsers a job downloads match these libraries at this version.
ENV MANADE_PLAYWRIGHT_VERSION=${PLAYWRIGHT_VERSION}
LABEL manade.runner-image=browser
USER runner

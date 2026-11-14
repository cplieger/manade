# check=error=true
FROM ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300 AS runner

# GitHub's image ships the runner, the .NET runtime and Node.js builds the
# runner bundles, its container hooks and Docker's static binaries outside
# apt, with no licence text. Each but the container hooks (below) is fetched
# at the version the binary reports, or copied from the runner's own tree, so
# a base digest bump keeps them in step.
FROM runner AS licenses
USER root
RUN install -d -o runner /out
USER runner
# The image records no version for its container hooks; this mirrors the
# RUNNER_CONTAINER_HOOKS_VERSION of actions/runner's images/Dockerfile
# (https://github.com/actions/runner/blob/v2.338.2/images/Dockerfile). Its
# second copy, 0.8.1 in k8s-novolume, carries the same MIT text.
ARG RUNNER_CONTAINER_HOOKS_VERSION=0.7.0
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# moby tags its engine releases docker-vX.Y.Z from v29 on
# (https://github.com/moby/moby/releases/tag/docker-v29.0.0).
RUN fetch() { \
        local dir=$1 base=$2 f; shift 2; [ "$#" -gt 0 ] || set -- LICENSE; mkdir -p "/out/$dir"; \
        for f in "$@"; do curl -fsSL --retry 3 -o "/out/$dir/$f" "https://raw.githubusercontent.com/$base/$f" || return; done; \
    } \
    && docker_v=$(docker --version | sed -E 's/^Docker version ([^,]+),.*/\1/') \
    && dotnet_v=$(jq -er '.runtimeOptions.includedFrameworks[] | select(.name == "Microsoft.NETCore.App") | .version' /home/runner/bin/Runner.Listener.runtimeconfig.json) \
    && fetch actions-runner "actions/runner/v$(/home/runner/bin/Runner.Listener --version)" \
    && fetch dotnet-runtime "dotnet/runtime/v${dotnet_v}" LICENSE.TXT THIRD-PARTY-NOTICES.TXT \
    && fetch runner-container-hooks "actions/runner-container-hooks/v${RUNNER_CONTAINER_HOOKS_VERSION}" \
    && fetch docker-cli "docker/cli/v${docker_v}" \
    && fetch moby "moby/moby/docker-v${docker_v}" \
    && fetch containerd "containerd/containerd/$(containerd --version | awk '{print $3}')" \
    && fetch runc "opencontainers/runc/v$(runc --version | awk 'NR == 1 {print $3}')" \
    && fetch tini "krallin/tini/v$(docker-init --version | awk '{print $3}')" \
    && fetch buildx "docker/buildx/$(/usr/local/lib/docker/cli-plugins/docker-buildx version | awk '{print $2}')" \
    && for d in /home/runner/externals/node*/; do install -D -m 644 "${d}LICENSE" "/out/nodejs/$(basename "$d")/LICENSE" || exit 1; done

FROM runner
USER root
ARG PKG_REFRESH=static
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# Only what a job cannot install without root and actions assume on a hosted
# runner: libatomic1 is linked by nodejs.org's Linux builds, which
# actions/setup-node installs, and envsubst is run by
# sigstore/cosign-installer.
# hadolint ignore=DL3008
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    gettext-base \
    git-lfs \
    gnupg \
    libatomic1 \
    openssh-client \
    xz-utils \
    zip \
    zstd \
    && rm -rf /var/lib/apt/lists/*
COPY --from=licenses /out/ /usr/share/licenses/
COPY LICENSE NOTICE /usr/share/licenses/manade-runner/
LABEL manade.runner-image=base
USER runner

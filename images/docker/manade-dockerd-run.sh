#!/bin/bash
# The docker image's runner command: start the image's Docker daemon through
# sudo, wait for it to answer, then run GitHub's runner, or the arguments when
# given any. A daemon that cannot start (sudo refused, no runtime for it)
# exits non-zero with its log, so the runner never registers.
set -euo pipefail

# Runs as root. cgroup v2 nesting as moby's hack/dind does it
# (https://github.com/moby/moby/blob/master/hack/dind): the container's
# processes leave its namespace root for /init, or enabling the controllers
# fails with EBUSY and no inner container can take a resource limit. A
# process started meanwhile lands in the root again, hence the retries.
# shellcheck disable=SC2329 # Invoked by name inside sudo's bash -c below.
start_dockerd() {
  if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    mkdir -p /sys/fs/cgroup/init
    local tries=0
    until {
      xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs || :
      sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers >/sys/fs/cgroup/cgroup.subtree_control
    }; do
      tries=$((tries + 1))
      if [ "$tries" -ge 10 ]; then
        printf 'manade-dockerd-run: enabling cgroup nesting failed, so inner containers cannot take resource limits\n' >&2
        break
      fi
    done
  fi
  exec dockerd
}

log="${TMPDIR:-/tmp}/manade-dockerd.log"
refuse() {
  printf 'manade-dockerd-run: dockerd %s; its log:\n' "$1" >&2
  tail -n 20 "$log" >&2
  exit 1
}

# shellcheck disable=SC2024 # The log is the runner user's, so it can read it.
sudo -n bash -c "$(declare -f start_dockerd); start_dockerd" >"$log" 2>&1 &
pid=$!
while [ "$SECONDS" -lt 60 ]; do
  if docker version >/dev/null 2>&1; then
    # Past here $log is never printed, so the wrapper's own lines go to the
    # container's log.
    grep -F 'manade-dockerd-run:' "$log" >&2 || :
    if [ "$#" -eq 0 ]; then
      set -- /home/runner/run.sh
    fi
    exec "$@"
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    refuse "exited before it answered"
  fi
  sleep 1
done
refuse "did not answer within 60 seconds"

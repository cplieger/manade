#!/bin/bash
# The docker image's smoke test, run by manade-dockerd-run in place of the
# runner, from internal/jobspec's TestImages. $1 is the storage the inner
# daemon must use: copy, overlay or any.
set -euo pipefail
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

[ "$(id -u):$(id -g)" = 1001:1001 ] || fail "runs as $(id -u):$(id -g), want 1001:1001"
out=$(docker run --rm hello-world 2>&1) || fail "docker run hello-world: $out"
grep -q 'Hello from Docker!' <<<"$out" || fail "hello-world printed: $out"
# Limits need the controllers the wrapper delegates under cgroup v2.
out=$(docker run --rm --memory 64m --cpus 0.5 --pids-limit 64 hello-world 2>&1) || fail "docker run with limits: $out"
grep -q 'Hello from Docker!' <<<"$out" || fail "hello-world with limits printed: $out"

# Engine 29's containerd image store names its snapshotters native and
# overlayfs where the classic store says vfs and overlay2
# (https://github.com/containerd/containerd/blob/main/docs/snapshotters/README.md).
driver=$(docker info --format '{{.Driver}}')
case "$1:$driver" in
  copy:vfs | copy:native | overlay:overlay2 | overlay:overlayfs | any:*) ;;
  *) fail "the inner daemon's storage driver is $driver, want $1" ;;
esac

printf 'manade-runner docker smoke: ok (%s)\n' "$driver"

#!/usr/bin/env bash
# The app image's own test: the binary in the shipped base refuses a missing
# credential, parses the example config, runs its healthcheck command and
# carries its licence tree. A real start needs GitHub, so none is attempted.
# Usage: tests/image-test.sh [IMAGE] (default: manade:smoke)
set -euo pipefail
cd "$(dirname "$0")/.."

IMAGE="${1:-manade:smoke}"
C_EMPTY="smoke-manade-empty-$$"
C_CONFIG="smoke-manade-config-$$"
C_HEALTH="smoke-manade-health-$$"
TREE=""

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  for c in "$C_EMPTY" "$C_CONFIG" "$C_HEALTH"; do
    if docker inspect "$c" >/dev/null 2>&1; then
      printf -- '--- %s logs (tail) ---\n' "$c" >&2
      docker logs "$c" 2>&1 | tail -40 >&2 || true
    fi
  done
  exit 1
}

# shellcheck disable=SC2317,SC2329 # invoked through the EXIT trap
cleanup() {
  docker rm -f "$C_EMPTY" "$C_CONFIG" "$C_HEALTH" >/dev/null 2>&1 || true
  if [ -n "$TREE" ]; then rm -rf "$TREE"; fi
}
trap cleanup EXIT

# run_to_exit NAME: starts the created container NAME attached, bounded, and
# sets rc to its exit status and out to what it wrote. The attached stream is
# read rather than docker logs, which can lag the exit.
run_to_exit() {
  rc=0
  out=$(timeout 60 docker start -a "$1" 2>&1) || rc=$?
  [ "$rc" -ne 124 ] || fail "$1 did not exit within 60 seconds"
}

command -v docker >/dev/null 2>&1 || fail "docker is required"
# The release job's image-test passes a registry digest it never pulls, and
# docker image inspect does not pull, so a missing image is pulled here.
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  pulled=$(timeout 300 docker pull "$IMAGE" 2>&1) \
    || fail "image $IMAGE is neither local nor pullable (docker build -t manade:smoke . first): $pulled"
fi

user=$(docker image inspect -f '{{.Config.User}}' "$IMAGE")
[ "$user" = 65532:65532 ] || fail "the image runs as '$user', want 65532:65532"

docker create --name "$C_EMPTY" --network none "$IMAGE" >/dev/null
run_to_exit "$C_EMPTY"
[ "$rc" -eq 1 ] || fail "with no environment manade exited $rc, want 1"
case "$out" in
  *'msg="manade stopped on a failure"'*GITHUB_APP_CLIENT_ID*) ;;
  *) fail "with no environment manade did not refuse the missing credential: $out" ;;
esac
printf 'manade image: no credential is refused with exit 1\n'

# The example config with a placeholder token passes the config layer, so
# the refusal moves to the Docker socket the container lacks.
docker create --name "$C_CONFIG" --network none \
  -e GITHUB_TOKEN=not-a-real-token -e CONFIG_FILE=/tmp/config.yaml "$IMAGE" >/dev/null
docker cp config.example.yaml "$C_CONFIG:/tmp/config.yaml" >/dev/null
run_to_exit "$C_CONFIG"
[ "$rc" -eq 1 ] || fail "with the example config manade exited $rc, want 1"
case "$out" in
  *'msg="manade starting"'*'msg="manade stopped on a failure"'*'DOCKER_SOCKET:'*) ;;
  *) fail "the example config did not reach the Docker check: $out" ;;
esac
case "$out" in
  *'error="config:'*) fail "the example config was refused: $out" ;;
esac
printf 'manade image: the example config parses and the start stops at the Docker socket\n'

# The image's own HEALTHCHECK command, run with no server. go/health's probe
# reads healthy when its marker's directory is unusable, so exit 1 here also
# proves that directory is usable as uid 65532.
mapfile -t probe < <(docker image inspect -f '{{range .Config.Healthcheck.Test}}{{println .}}{{end}}' "$IMAGE" | sed '/^$/d')
if [ "${#probe[@]}" -lt 2 ] || [ "${probe[0]}" != CMD ]; then
  fail "the HEALTHCHECK is not in exec form: ${probe[*]:-none}"
fi
docker create --name "$C_HEALTH" --network none --entrypoint "${probe[1]}" "$IMAGE" "${probe[@]:2}" >/dev/null
run_to_exit "$C_HEALTH"
[ "$rc" -eq 1 ] || fail "the healthcheck with no server exited $rc, want 1"
case "$out" in
  *"unhealthy: marker absent"*) ;;
  *) fail "the healthcheck with no server did not report the absent marker: $out" ;;
esac
printf 'manade image: the healthcheck reads unhealthy before the server writes its marker\n'

TREE=$(mktemp -d)
docker cp "$C_EMPTY:/usr/share/licenses" "$TREE/" >/dev/null || fail "the image has no readable /usr/share/licenses"
for f in manade/LICENSE manade/THIRD_PARTY_NOTICES.md go/LICENSE github.com/actions/scaleset/LICENSE; do
  [ -s "$TREE/licenses/$f" ] || fail "the licence tree lacks $f"
done
printf 'manade image: the licence tree holds manade, Go and scaleset\n'

printf 'manade image smoke: ok\n'

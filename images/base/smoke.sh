#!/bin/bash
# The base image's smoke test, run as the container's command by
# internal/jobspec's TestImages under the default profile.
set -euo pipefail
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

[ "$(id -u):$(id -g)" = 1001:1001 ] || fail "runs as $(id -u):$(id -g), want 1001:1001"
if sudo -n true 2>/dev/null; then
  fail "sudo works under the default profile"
fi
for d in manade-runner actions-runner runner-container-hooks docker-cli moby containerd runc tini buildx; do
  [ -s "/usr/share/licenses/$d/LICENSE" ] || fail "no licence text for $d"
done
[ -s /usr/share/licenses/manade-runner/NOTICE ] || fail "no NOTICE for manade-runner"
for f in LICENSE.TXT THIRD-PARTY-NOTICES.TXT; do
  [ -s "/usr/share/licenses/dotnet-runtime/$f" ] || fail "no $f for the runner's .NET runtime"
done
# An unmatched glob stays literal, so a runner without Node builds fails too.
for d in /home/runner/externals/node*/; do
  [ -s "/usr/share/licenses/nodejs/$(basename "$d")/LICENSE" ] || fail "no licence text for the runner's $(basename "$d")"
done

w=$(mktemp -d)
cd "$w"
head -c 1048576 /dev/urandom >in
{ zstd -q in -o in.zst && zstd -dq in.zst -o out.zst && cmp in out.zst; } || fail "zstd round trip"
{ xz -kc in >in.xz && xz -dc in.xz >out.xz && cmp in out.xz; } || fail "xz round trip"
{ zip -q in.zip in && unzip -p in.zip in >out.zip && cmp in out.zip; } || fail "zip round trip"
export GNUPGHOME="$w/gnupg"
mkdir -m 700 "$GNUPGHOME"
gpg -q --batch --pinentry-mode loopback --passphrase smoke -o in.gpg -c in || fail "gpg encrypt"
{ gpg -q --batch --pinentry-mode loopback --passphrase smoke -o out.gpg -d in.gpg && cmp in out.gpg; } || fail "gpg round trip"
envsubst --version >/dev/null || fail "envsubst --version"
# shellcheck disable=SC2016 # envsubst expands the literal $X itself.
[ "$(printf '$X' | X=smoke envsubst)" = smoke ] || fail "envsubst did not substitute"
{ git init -q lfs && git -C lfs lfs track '*.bin' >/dev/null && grep -q 'filter=lfs' lfs/.gitattributes; } || fail "git lfs track"
ssh -V 2>&1 | grep -q OpenSSH || fail "ssh is not OpenSSH"

# What actions/setup-node does: nodejs.org's newest LTS build, run as uid 1001.
case "$(uname -m)" in x86_64) arch=x64 ;; aarch64) arch=arm64 ;; *) fail "no Node build for $(uname -m)" ;; esac
v=$(curl -fsSL https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts != false)][0].version')
tarball="node-$v-linux-$arch.tar.xz"
curl -fsSLO "https://nodejs.org/dist/$v/$tarball"
curl -fsSL "https://nodejs.org/dist/$v/SHASUMS256.txt" | grep " $tarball\$" | sha256sum -c --quiet || fail "node checksum"
mkdir node && tar -xJf "$tarball" -C node --strip-components=1
[ "$(node/bin/node -e 'console.log(6 * 7)')" = 42 ] || fail "node $v did not run"

printf 'manade-runner base smoke: ok\n'

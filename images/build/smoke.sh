#!/bin/bash
# The build image's smoke test, run as the container's command by
# internal/jobspec's TestImages under the default profile.
set -euo pipefail
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

[ "$(id -u):$(id -g)" = 1001:1001 ] || fail "runs as $(id -u):$(id -g), want 1001:1001"
w=$(mktemp -d)
cd "$w"

cat >libs.c <<'C'
#include <openssl/crypto.h>
#include <stdio.h>
#include <zlib.h>
int main(void) { printf("%s %s\n", OpenSSL_version(OPENSSL_VERSION), zlibVersion()); return 0; }
C
# shellcheck disable=SC2046 # pkg-config's flags are separate words.
gcc -o libs libs.c $(pkg-config --cflags --libs openssl zlib) || fail "gcc against libssl and zlib"
./libs | grep -q '^OpenSSL ' || fail "the program linked against libssl did not run"

# What actions/setup-go does: go.dev's newest release, run as uid 1001.
arch=$(dpkg --print-architecture)
read -r v sum < <(curl -fsSL 'https://go.dev/dl/?mode=json' \
  | jq -r --arg a "$arch" '.[0] as $r | $r.files[] | select(.os == "linux" and .arch == $a and .kind == "archive") | "\($r.version) \(.sha256)"')
curl -fsSL -o go.tgz "https://go.dev/dl/$v.linux-$arch.tar.gz"
printf '%s  go.tgz\n' "$sum" | sha256sum -c --quiet || fail "go checksum"
tar -xzf go.tgz
export PATH="$w/go/bin:$PATH" GOPATH="$w/gopath" GOCACHE="$w/gocache" GOTOOLCHAIN=local GOFLAGS='' CGO_ENABLED=1

mkdir mod && cd mod
printf 'module smoke\n\ngo 1.25\n' >go.mod
cat >twice.go <<'GO'
package smoke

// static int twice(int x) { return 2 * x; }
import "C"

func Twice(x int) int { return int(C.twice(C.int(x))) }
GO
cat >twice_test.go <<'GO'
package smoke

import (
	"sync"
	"testing"
)

func TestTwice(t *testing.T) {
	if got := Twice(21); got != 42 {
		t.Fatalf("Twice(21) = %d", got)
	}
}

func TestRacy(t *testing.T) {
	var n int
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { n++ })
	}
	wg.Wait()
	_ = n
}
GO
go test -race -run TestTwice . || fail "go test -race on a cgo package"
if out=$(go test -race -run TestRacy . 2>&1); then
  fail "the race detector missed a data race"
fi
grep -q 'WARNING: DATA RACE' <<<"$out" || fail "go test -race failed without a race report: $out"

printf 'manade-runner build smoke: ok\n'

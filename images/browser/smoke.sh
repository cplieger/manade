#!/bin/bash
# The browser image's smoke test, run as the container's command by
# internal/jobspec's TestImages under the default profile.
set -euo pipefail
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

[ "$(id -u):$(id -g)" = 1001:1001 ] || fail "runs as $(id -u):$(id -g), want 1001:1001"
[ -n "${MANADE_PLAYWRIGHT_VERSION:-}" ] || fail "the image names no Playwright version"
command -v Xvfb >/dev/null || fail "no Xvfb"
w=$(mktemp -d)
cd "$w"

case "$(uname -m)" in x86_64) arch=x64 ;; aarch64) arch=arm64 ;; *) fail "no Node build for $(uname -m)" ;; esac
v=$(curl -fsSL https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts != false)][0].version')
tarball="node-$v-linux-$arch.tar.xz"
curl -fsSLO "https://nodejs.org/dist/$v/$tarball"
curl -fsSL "https://nodejs.org/dist/$v/SHASUMS256.txt" | grep " $tarball\$" | sha256sum -c --quiet || fail "node checksum"
mkdir node && tar -xJf "$tarball" -C node --strip-components=1
export PATH="$w/node/bin:$PATH" npm_config_cache="$w/npm" PLAYWRIGHT_BROWSERS_PATH="$w/browsers"

# Without --with-deps: the libraries must already be in the image.
npm install -s --no-save "playwright@$MANADE_PLAYWRIGHT_VERSION" || fail "npm install playwright"
npx playwright install chromium || fail "playwright install chromium"
title=$(node -e '
const { chromium } = require("playwright");
(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage();
  await page.setContent("<title>smoke</title>");
  console.log(await page.title());
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
') || fail "headless Chromium did not launch"
[ "$title" = smoke ] || fail "headless Chromium read the title as '$title'"

printf 'manade-runner browser smoke: ok\n'

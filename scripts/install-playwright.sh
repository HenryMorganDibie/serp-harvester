#!/bin/sh
# Installs the Playwright driver and Chromium for mode: playwright.
#
# playwright-go v0.6000.0 (pinned in go.mod) downloads its driver from
# playwright*.azureedge.net, which no longer serves the 1.60.0 driver (404).
# The driver is just the playwright-core npm package run by Node.js, so this
# script assembles it from the npm registry (checksum-pinned), applies the
# same bundle patch playwright-go applies after its own download, and lets
# Playwright's official CLI install Chromium from its current CDN.
#
# Usage: scripts/install-playwright.sh [--with-deps]
#   --with-deps  also install Chromium's system libraries (apt; uses sudo
#                when not root)
#
# Honors PLAYWRIGHT_DRIVER_PATH and PLAYWRIGHT_BROWSERS_PATH, with the same
# defaults playwright-go and Playwright use, so the harvester finds what
# this installs. Requires Node.js >= 18 and curl.
set -eu

# Must equal playwrightCliVersion in the playwright-go version in go.mod;
# internal/fetcher/playwright_test.go checks this.
PLAYWRIGHT_VERSION=1.60.0
PLAYWRIGHT_CORE_SHA512=f5b5bacef5ff9b49446e04ca27a629a4e2b1f07dd538f04c382161da2ad714e4f805b1e0af1e613e3c0960b4f8d0bbbee2ab43dfaa8a73f1e7e7a4ad516e7b20

WITH_DEPS=""
case "${1:-}" in
  "") ;;
  --with-deps) WITH_DEPS="--with-deps" ;;
  *) echo "usage: $0 [--with-deps]" >&2; exit 2 ;;
esac

NODE=$(command -v node) || { echo "install-playwright: Node.js >= 18 is required" >&2; exit 1; }
DRIVER_DIR=${PLAYWRIGHT_DRIVER_PATH:-$HOME/.cache/ms-playwright-go/$PLAYWRIGHT_VERSION}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "install-playwright: driver $PLAYWRIGHT_VERSION -> $DRIVER_DIR"
curl -fsSL -o "$TMP/core.tgz" \
  "https://registry.npmjs.org/playwright-core/-/playwright-core-$PLAYWRIGHT_VERSION.tgz"
if command -v sha512sum >/dev/null; then
  SUM=$(sha512sum "$TMP/core.tgz")
else
  SUM=$(shasum -a 512 "$TMP/core.tgz") # macOS
fi
[ "${SUM%% *}" = "$PLAYWRIGHT_CORE_SHA512" ] \
  || { echo "install-playwright: playwright-core checksum mismatch" >&2; exit 1; }

mkdir -p "$DRIVER_DIR"
rm -rf "$DRIVER_DIR/package"
tar -xzf "$TMP/core.tgz" -C "$DRIVER_DIR" # the tarball's root is package/
ln -sf "$NODE" "$DRIVER_DIR/node"          # playwright-go runs <driver dir>/node

# playwright-go's patchDriverBundle: tolerate page errors without a source
# location instead of crashing the driver. (Portable, no sed -i.)
BUNDLE="$DRIVER_DIR/package/lib/coreBundle.js"
sed \
  -e 's#pageError\.location\.url#pageError.location?.url || ""#g' \
  -e 's#pageError\.location\.lineNumber#pageError.location?.lineNumber || 0#g' \
  -e 's#pageError\.location\.columnNumber#pageError.location?.columnNumber || 0#g' \
  "$BUNDLE" > "$TMP/coreBundle.js"
mv "$TMP/coreBundle.js" "$BUNDLE"

"$DRIVER_DIR/node" "$DRIVER_DIR/package/cli.js" --version | grep -q "$PLAYWRIGHT_VERSION" \
  || { echo "install-playwright: driver does not report $PLAYWRIGHT_VERSION" >&2; exit 1; }

if [ "${PLAYWRIGHT_SKIP_BROWSER_INSTALL:-}" = "1" ]; then
  echo "install-playwright: PLAYWRIGHT_SKIP_BROWSER_INSTALL=1, not installing Chromium"
  exit 0
fi
# Installs Chromium and the headless shell the fetcher uses by default.
"$DRIVER_DIR/node" "$DRIVER_DIR/package/cli.js" install $WITH_DEPS chromium

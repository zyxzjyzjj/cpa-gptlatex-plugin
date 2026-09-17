#!/usr/bin/env bash
# Package release assets in the layout the CLIProxyAPI plugin store requires.
#
# Produces, in release-assets/:
#   prism-provider_<version>_<goos>_<goarch>.zip
#   checksums.txt
#
# Installer requirements this script satisfies:
#   - one zip per platform, named <id>_<version>_<goos>_<goarch>.zip, <version>
#     being the release tag WITHOUT the leading v
#   - the dynamic library at the zip ROOT, named <id>.<ext>; nested libraries,
#     extra files, absolute paths and zip-slip entries are rejected
#   - checksums.txt in "<sha256>  <bare-filename>" form. A "./" prefix parses but
#     then fails lookup with "checksum not found", so names must be bare.
#
# Usage:  bash tools/package-release.sh 0.1.0
#
# Cross-compiling needs a matching C toolchain (the plugin ABI is a C ABI). Set
# CC_<goos>_<goarch> per target, e.g. CC_linux_amd64=gcc or
# CC_windows_amd64=x86_64-w64-mingw32-gcc. Platforms that fail to build are
# skipped, which is how the off-platform runs stay usable.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "usage: $0 <version>   e.g. $0 0.1.0" >&2
  exit 1
fi
VERSION="${VERSION#v}"   # the store rejects a leading v in asset names

PLUGIN_ID="prism-provider"
MODULE_DIR="."   # the module IS the repo root
OUT_DIR="$PWD/release-assets"
PLATFORMS="${PLATFORMS:-windows/amd64 linux/amd64}"

# The release tag, the compiled-in version and registry.json must agree, or the
# published listing would advertise a version the binary does not report.
source_version="$(sed -n 's/^var version = "\([^"]*\)"/\1/p' "$MODULE_DIR/main.go")"
registry_version="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' registry.json | head -1)"
for pair in "source:$source_version" "registry:$registry_version"; do
  if [ "${pair#*:}" != "$VERSION" ]; then
    echo "ERROR: version mismatch (${pair}, requested $VERSION)" >&2
    exit 1
  fi
done

zip_tool=""
if command -v zip >/dev/null 2>&1; then
  zip_tool="zip"
else
  for candidate in python3 python py; do
    if command -v "$candidate" >/dev/null 2>&1 &&
       "$candidate" -c 'import zipfile' >/dev/null 2>&1; then
      zip_tool="$candidate"; break
    fi
  done
fi
[ -n "$zip_tool" ] || { echo "ERROR: no zip tool (install zip(1) or Python 3)" >&2; exit 1; }

mkdir -p "$OUT_DIR"

for platform in $PLATFORMS; do
  GOOS_TARGET="${platform%%/*}"
  GOARCH_TARGET="${platform##*/}"
  case "$GOOS_TARGET" in
    windows) EXT="dll" ;;
    darwin)  EXT="dylib" ;;
    *)       EXT="so" ;;
  esac
  LIB_NAME="${PLUGIN_ID}.${EXT}"
  ZIP_NAME="${PLUGIN_ID}_${VERSION}_${GOOS_TARGET}_${GOARCH_TARGET}.zip"

  cc_var="CC_${GOOS_TARGET}_${GOARCH_TARGET}"
  if [ -n "${!cc_var:-}" ]; then export CC="${!cc_var}"; fi

  echo "==> building ${GOOS_TARGET}/${GOARCH_TARGET}"
  stage="$(mktemp -d)"
  rm -f "$OUT_DIR/$ZIP_NAME"    # never leave a stale zip beside a fresh checksum
  if ! ( cd "$MODULE_DIR" && CGO_ENABLED=1 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
           go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o "$stage/$LIB_NAME" . ); then
    echo "    skipped ${GOOS_TARGET}/${GOARCH_TARGET} (no C toolchain for this target)" >&2
    rm -rf "$stage"; continue
  fi

  if [ "$zip_tool" = "zip" ]; then
    ( cd "$stage" && zip -q -X "$OUT_DIR/$ZIP_NAME" "$LIB_NAME" )
  else
    LIB_NAME="$LIB_NAME" ZIP_PATH="$OUT_DIR/$ZIP_NAME" STAGE="$stage" "$zip_tool" -c '
import os, zipfile
stage, lib, out = os.environ["STAGE"], os.environ["LIB_NAME"], os.environ["ZIP_PATH"]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
    zf.write(os.path.join(stage, lib), arcname=lib)   # root, as the installer requires
'
  fi
  rm -rf "$stage"
  echo "    $ZIP_NAME"
done

current=("$OUT_DIR/${PLUGIN_ID}_${VERSION}_"*.zip)
[ -e "${current[0]}" ] || { echo "no platform was built for $VERSION; nothing to package" >&2; exit 1; }

OUT_DIR="$OUT_DIR" PREFIX="${PLUGIN_ID}_${VERSION}_" "$zip_tool" -c "$(cat <<'PYSUM'
import hashlib, os
out_dir, prefix = os.environ["OUT_DIR"], os.environ["PREFIX"]
lines = []
for name in sorted(n for n in os.listdir(out_dir) if n.startswith(prefix) and n.endswith(".zip")):
    digest = hashlib.sha256()
    with open(os.path.join(out_dir, name), "rb") as fh:
        for block in iter(lambda: fh.read(1 << 20), b""):
            digest.update(block)
    lines.append("%s  %s" % (digest.hexdigest(), name))   # bare file name
with open(os.path.join(out_dir, "checksums.txt"), "w", newline="\n") as fh:
    fh.write("\n".join(lines) + "\n")
PYSUM
)"

echo
echo "release assets in $OUT_DIR:"
ls -la "$OUT_DIR"
echo
echo "Attach ${PLUGIN_ID}_${VERSION}_*.zip plus checksums.txt to the GitHub release tagged v$VERSION."

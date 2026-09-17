#!/usr/bin/env bash
# Build the CPA plugin shared library.
#
# The plugin is a cgo "c-shared" artifact because that is the only shape
# internal/pluginhost/loader_windows.go knows how to load: it LoadLibrary()s the
# file and looks up the cliproxy_plugin_init symbol.
set -euo pipefail
cd "$(dirname "$0")"

# cgo needs a C compiler. Prefer CC, then PATH, then the portable MinGW-w64
# toolchain used to bring this repo up (see README).
if [ -n "${CC:-}" ] && command -v "${CC%% *}" >/dev/null 2>&1; then
  :
elif command -v gcc >/dev/null 2>&1; then
  export CC=gcc
elif [ -x /c/zjj/toolchain/mingw64/bin/gcc.exe ]; then
  export CC=/c/zjj/toolchain/mingw64/bin/gcc.exe
else
  echo "找不到 C 编译器：安装 MinGW-w64 后重试，或用 CC=/path/to/gcc 指定" >&2
  exit 1
fi

case "$(uname -s)" in
  Darwin) EXT=dylib ;;
  MINGW*|MSYS*|CYGWIN*) EXT=dll ;;
  *) EXT=so ;;
esac

# Only inject a version when one can actually be derived: this directory is
# often not a git checkout, and overriding main.version with a placeholder
# would be worse than leaving the compiled-in default in place.
LDFLAGS=()
if VERSION="$(git describe --tags --always --dirty 2>/dev/null)" && [ -n "$VERSION" ]; then
  LDFLAGS=(-ldflags "-X main.version=$VERSION")
fi

echo "CC=${CC}  ->  prism-provider.${EXT}"
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
  ${LDFLAGS[@]+"${LDFLAGS[@]}"} \
  -o "prism-provider.${EXT}" .
rm -f prism-provider.h
echo "built prism-provider.${EXT} ($(du -h "prism-provider.${EXT}" | cut -f1))"

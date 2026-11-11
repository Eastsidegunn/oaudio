#!/bin/sh
# Copies the shared libraries the sherpa-onnx runner links against into
# <out>/lib and makes the runner look for them there, instead of in the Go
# module cache of the machine that built it. macOS only for now.
set -eu

OUT="$1"
BIN="$OUT/oaudio-runner-sherpa-onnx"
[ "$(uname -s)" = "Darwin" ] || { echo "bundle-libs: only macOS is handled; the runner still uses the module cache" >&2; exit 0; }

case "$(uname -m)" in
  arm64) TRIPLE=aarch64-apple-darwin ;;
  *)     TRIPLE=x86_64-apple-darwin ;;
esac
MODULE="$(go list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-macos/v2)"
[ -d "$MODULE/lib/$TRIPLE" ] || { echo "bundle-libs: no libraries for $TRIPLE in $MODULE" >&2; exit 1; }

mkdir -p "$OUT/lib"
for lib in "$MODULE/lib/$TRIPLE"/*.dylib; do
  install -m 0644 "$lib" "$OUT/lib/"
done

otool -l "$BIN" | awk '/LC_RPATH/ { getline; getline; print $2 }' | while read -r old; do
  install_name_tool -delete_rpath "$old" "$BIN"
done
install_name_tool -add_rpath @loader_path/lib "$BIN"
# Editing a binary invalidates its signature; Apple Silicon needs one to run.
codesign --force --sign - "$BIN" >/dev/null 2>&1 || true

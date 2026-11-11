#!/bin/sh
# Copies the shared libraries the sherpa-onnx runner links against into
# <out>/lib and makes the runner look for them there, instead of in the Go
# module cache of the machine that built it, so the pair can be copied to
# another machine. Linux needs patchelf.
set -eu

OUT="$1"
BIN="$OUT/oaudio-runner-sherpa-onnx"

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  MOD=sherpa-onnx-go-macos TRIPLE=aarch64-apple-darwin ;;
  Darwin-*)      MOD=sherpa-onnx-go-macos TRIPLE=x86_64-apple-darwin ;;
  Linux-x86_64)  MOD=sherpa-onnx-go-linux TRIPLE=x86_64-unknown-linux-gnu ;;
  Linux-aarch64) MOD=sherpa-onnx-go-linux TRIPLE=aarch64-unknown-linux-gnu ;;
  *) echo "bundle-libs: $(uname -s) $(uname -m) is not handled; the runner still uses the module cache" >&2; exit 0 ;;
esac
MODULE="$(go list -m -f '{{.Dir}}' "github.com/k2-fsa/$MOD/v2")"
[ -d "$MODULE/lib/$TRIPLE" ] || { echo "bundle-libs: no libraries for $TRIPLE in $MODULE" >&2; exit 1; }

mkdir -p "$OUT/lib"
for lib in "$MODULE/lib/$TRIPLE"/*; do
  install -m 0644 "$lib" "$OUT/lib/"
done

if [ "$(uname -s)" = Linux ]; then
  command -v patchelf >/dev/null || { echo "bundle-libs: patchelf is needed on Linux" >&2; exit 1; }
  patchelf --set-rpath '$ORIGIN/lib' "$BIN"
  for lib in "$OUT"/lib/*.so*; do
    patchelf --set-rpath '$ORIGIN' "$lib"
  done
  exit 0
fi

otool -l "$BIN" | awk '/LC_RPATH/ { getline; getline; print $2 }' | while read -r old; do
  install_name_tool -delete_rpath "$old" "$BIN"
done
install_name_tool -add_rpath @loader_path/lib "$BIN"
# Editing a binary invalidates its signature; Apple Silicon needs one to run.
codesign --force --sign - "$BIN" >/dev/null 2>&1 || true

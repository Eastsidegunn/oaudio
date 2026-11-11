#!/bin/sh
# Builds a release archive for this machine's OS and architecture:
#
#   dist/oaudio-<os>-<arch>.tar.gz          the daemon, CLI, helpers and every runner
#   dist/oaudio-<os>-<arch>.tar.gz.sha256
#
# The archive unpacks to one directory that runs from wherever it is put:
# runners are found next to the oaudio binary, and their shared libraries in
# lib/ beside them. Engines are built from scratch, so a stale cmake cache
# with other settings cannot leak into a release.
set -eu
cd "$(dirname "$0")/.."

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) echo "package: $(uname -s) is not supported" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) echo "package: $(uname -m) is not supported" >&2; exit 1 ;;
esac
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
NAME="oaudio-$OS-$ARCH"
STAGE="dist/$NAME"

rm -rf "$STAGE" "dist/$NAME.tar.gz" "dist/$NAME.tar.gz.sha256"
mkdir -p "$STAGE"
make -C runners/whisper clean
make -C runners/audiocpp clean
make -C runners/speechcpp clean
make BIN="$STAGE" VERSION="$VERSION" all runners
cp LICENSE NOTICE "$STAGE/"
printf '%s\n' "$VERSION" > "$STAGE/VERSION"

# Everything in the archive must run on a machine without the build tree.
if [ "$OS" = darwin ]; then
  for f in "$STAGE"/oaudio* "$STAGE"/audiocpp_server "$STAGE"/speechcpp; do
    if otool -L "$f" | tail -n +2 | grep -vE '^\s*(/usr/lib/|/System/|@rpath/lib(sherpa-onnx|onnxruntime))'; then
      echo "package: $f links a library that is not shipped" >&2; exit 1
    fi
  done
else
  # whisper.cpp's Go bindings link libgomp on Linux, which minimal systems
  # lack. It is shipped in lib/ like the sherpa-onnx libraries.
  gomp="$(ldd "$STAGE/oaudio-runner-whisper.cpp" | awk '/libgomp/ {print $3}')"
  if [ -n "$gomp" ]; then
    mkdir -p "$STAGE/lib"
    install -m 0644 "$gomp" "$STAGE/lib/"
    patchelf --set-rpath '$ORIGIN/lib' "$STAGE/oaudio-runner-whisper.cpp"
  fi
  for f in "$STAGE"/oaudio* "$STAGE"/audiocpp_server "$STAGE"/speechcpp; do
    if ldd "$f" 2>/dev/null | grep 'not found'; then
      echo "package: $f links a library that is not shipped" >&2; exit 1
    fi
  done
fi

tar -C dist -czf "dist/$NAME.tar.gz" "$NAME"
(cd dist && if command -v sha256sum >/dev/null; then sha256sum "$NAME.tar.gz"; else shasum -a 256 "$NAME.tar.gz"; fi) > "dist/$NAME.tar.gz.sha256"
echo "package: dist/$NAME.tar.gz ($VERSION)"

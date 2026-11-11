#!/bin/sh
# Installs oaudio from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/Eastsidegunn/oaudio/main/scripts/install.sh | sh
#
# Settings, all optional:
#   OAUDIO_VERSION   a release tag such as v0.1.0 (default: the latest release)
#   OAUDIO_PREFIX    where to install (default: ~/.local). The files go to
#                    $OAUDIO_PREFIX/lib/oaudio and the command to
#                    $OAUDIO_PREFIX/bin/oaudio. Nothing outside it is touched.
#   OAUDIO_DOWNLOAD  a different place to download releases from
#
# To uninstall: rm -rf "$OAUDIO_PREFIX/lib/oaudio" "$OAUDIO_PREFIX/bin/oaudio".
# Downloaded models are in ~/.oaudio.
set -eu

say() { printf '%s\n' "$*" >&2; }
die() { say "oaudio install: $*"; exit 1; }

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux)  OS=linux ;;
  *) die "$(uname -s) is not supported; see the README to build from source" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) die "$(uname -m) is not supported" ;;
esac
if [ "$OS" = darwin ]; then
  [ "$ARCH" = arm64 ] || die "Intel Macs are not supported; see the README to build from source"
  major="$(sw_vers -productVersion | cut -d. -f1)"
  [ "$major" -ge 14 ] || die "macOS 14 or later is needed (this is $(sw_vers -productVersion))"
fi
command -v curl >/dev/null || die "curl is needed"
command -v tar >/dev/null || die "tar is needed"

NAME="oaudio-$OS-$ARCH"
BASE="${OAUDIO_DOWNLOAD:-https://github.com/Eastsidegunn/oaudio/releases}"
if [ -n "${OAUDIO_VERSION:-}" ]; then
  URL="$BASE/download/$OAUDIO_VERSION/$NAME.tar.gz"
else
  URL="$BASE/latest/download/$NAME.tar.gz"
fi
PREFIX="${OAUDIO_PREFIX:-$HOME/.local}"
DEST="$PREFIX/lib/oaudio"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
say "Downloading $URL"
curl -fL --progress-bar -o "$TMP/$NAME.tar.gz" "$URL" || die "download failed"
curl -fsSL -o "$TMP/$NAME.tar.gz.sha256" "$URL.sha256" || die "checksum download failed"

want="$(awk '{print $1}' "$TMP/$NAME.tar.gz.sha256")"
if command -v sha256sum >/dev/null; then
  got="$(sha256sum "$TMP/$NAME.tar.gz" | awk '{print $1}')"
else
  got="$(shasum -a 256 "$TMP/$NAME.tar.gz" | awk '{print $1}')"
fi
[ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch: expected $want, got $got"

tar -C "$TMP" -xzf "$TMP/$NAME.tar.gz"
[ -x "$TMP/$NAME/oaudio" ] || die "the archive does not contain oaudio"

# Replace the previous install as a whole, so no runner from an older
# version is left next to the new daemon.
mkdir -p "$PREFIX/lib" "$PREFIX/bin"
rm -rf "$DEST.old"
[ -e "$DEST" ] && mv "$DEST" "$DEST.old"
mv "$TMP/$NAME" "$DEST"
rm -rf "$DEST.old"
ln -sf "$DEST/oaudio" "$PREFIX/bin/oaudio"

say "Installed oaudio $(cat "$DEST/VERSION" 2>/dev/null) to $DEST"
case ":$PATH:" in
  *":$PREFIX/bin:"*) ;;
  *) say "Add $PREFIX/bin to your PATH, for example:  export PATH=\"$PREFIX/bin:\$PATH\"" ;;
esac
command -v ffmpeg >/dev/null || say "Optional: install ffmpeg for mp3, opus, aac and flac, and for uploads in those formats."
say "Try:  oaudio run whisper recording.wav   (the server starts on first use; models download on first run)"

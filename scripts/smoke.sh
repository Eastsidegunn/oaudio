#!/bin/sh
# Runs an installed oaudio the way a new user would, with nothing on PATH but
# the system directories: speaks a sentence with Supertonic (sherpa-onnx),
# transcribes it back with Whisper (whisper.cpp), and checks that the
# audio.cpp server starts. Downloads about 180 MB of models.
#
#   scripts/smoke.sh <home>     where <home>/.local/bin/oaudio was installed
set -eu

H="$(cd "$1" && pwd)"
WORK="$(mktemp -d)"
PORT=11599
run() { env -i HOME="$H" PATH="$H/.local/bin:/usr/bin:/bin" OAUDIO_HOST="127.0.0.1:$PORT" "$@"; }
fail() { echo "smoke: $*" >&2; exit 1; }

# Started directly, not through run(), so that $! is the server itself.
env -i HOME="$H" PATH="$H/.local/bin:/usr/bin:/bin" OAUDIO_HOST="127.0.0.1:$PORT" "$H/.local/bin/oaudio" serve > "$WORK/serve.log" 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true; wait $SERVER 2>/dev/null || true; rm -rf "$WORK"' EXIT
i=0
# Poll with curl: an oaudio command would start a server of its own.
until curl -fs "http://127.0.0.1:$PORT/api/version" >/dev/null; do
  i=$((i + 1)); [ $i -lt 30 ] || { cat "$WORK/serve.log"; fail "server did not start"; }
  sleep 1
done

# After the wait: these commands would otherwise start a server of their own.
run oaudio version
run oaudio catalog >/dev/null || fail "catalog"
run oaudio pull supertonic
run oaudio pull whisper:base.en-q5_1
run oaudio run supertonic -o "$WORK/fox.wav" "The quick brown fox jumps over the lazy dog." || { cat "$WORK/serve.log"; fail "synthesis"; }
heard="$(run oaudio run whisper:base.en-q5_1 "$WORK/fox.wav")" || { cat "$WORK/serve.log"; fail "transcription"; }
echo "heard: $heard"
echo "$heard" | grep -qi "brown fox" || fail "the transcript does not match what was spoken"

# The audio.cpp server has no small model to load; check that it links and starts.
out="$("$H/.local/lib/oaudio/audiocpp_server" --help 2>&1 || true)"
echo "$out" | grep -qiE 'error while loading|library not loaded|not found' && { echo "$out"; fail "audiocpp_server does not start"; }

run oaudio stop supertonic >/dev/null 2>&1 || true
kill $SERVER; wait $SERVER 2>/dev/null || true
sleep 1
if curl -fs "http://127.0.0.1:$PORT/api/version" >/dev/null; then fail "a server is still running after stop"; fi
echo "smoke: ok"

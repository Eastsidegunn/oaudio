#!/bin/sh
# End-to-end check of the built binary: starts a private server (its own home
# directory and port, so it never touches a real install), speaks a sentence,
# transcribes it back, and checks lifecycle commands.
#
# Uses the engines built into macOS, so it needs macOS 26 or later.
set -eu

BIN="$(cd "$(dirname "$0")/.." && pwd)/bin/oaudio"
[ -x "$BIN" ] || { echo "build first: make" >&2; exit 1; }
[ "$(uname -s)" = "Darwin" ] || { echo "e2e needs macOS (system engines); skipping" >&2; exit 0; }
[ "$(sw_vers -productVersion | cut -d. -f1)" -ge 26 ] || { echo "e2e needs macOS 26+ for on-device recognition; skipping" >&2; exit 0; }

WORK="$(mktemp -d)"
export OAUDIO_HOME="$WORK/home"
export OAUDIO_HOST="127.0.0.1:$((20000 + $$ % 20000))"
"$BIN" serve >"$WORK/server.log" 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null || true; wait $SERVER 2>/dev/null || true; rm -rf "$WORK"' EXIT
for _ in $(seq 50); do curl -sf "http://$OAUDIO_HOST/api/version" >/dev/null 2>&1 && break; sleep 0.1; done

fail() { echo "FAIL: $*" >&2; echo "--- server log" >&2; tail -20 "$WORK/server.log" >&2; exit 1; }
pass() { echo "ok    $*"; }

"$BIN" list | grep -q "apple-say" || fail "apple-say not listed"
pass "system models are listed without a download"

TEXT="The quick brown fox jumps over the lazy dog."
"$BIN" run apple-say --voice Samantha -o "$WORK/en.wav" "$TEXT"
[ "$(head -c 4 "$WORK/en.wav")" = "RIFF" ] || fail "speech output is not a WAV file"
pass "text to speech wrote a WAV file"

HEARD="$("$BIN" run apple-speech "$WORK/en.wav" --language en)"
[ "$HEARD" = "$TEXT" ] || fail "round trip: said '$TEXT', heard '$HEARD'"
pass "speech to text heard exactly what was said"

"$BIN" run apple-speech "$WORK/en.wav" --language en --format srt | grep -q -- "-->" || fail "no SRT timestamps"
pass "subtitles carry timestamps"

echo "Piped text is spoken too." | "$BIN" run apple-say -o "$WORK/pipe.wav"
[ -s "$WORK/pipe.wav" ] || fail "piped input produced nothing"
pass "text can be piped in"

"$BIN" ps | grep -q "apple-speech" || fail "model not shown as loaded"
"$BIN" stop apple-speech
"$BIN" ps | grep -q "apple-speech" && fail "model still loaded after stop"
pass "models load on demand and unload on stop"

"$BIN" run whisper:nope x.wav 2>&1 | grep -q "no tag" || fail "bad tag not explained"
"$BIN" run apple-say --voice Nobody -o "$WORK/x.wav" "x" 2>&1 | grep -q "unknown voice" || fail "bad voice not explained"
pass "mistakes get a clear message"

kill $SERVER; wait $SERVER 2>/dev/null || true
sleep 0.5
if pgrep -f "oaudio runner .*$WORK" >/dev/null 2>&1; then fail "runner processes outlived the server"; fi
pass "stopping the server stops its runners"
echo "all end-to-end checks passed"

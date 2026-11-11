# ADR 0007: speech.cpp is driven through its worker, not its server

Status: accepted, 2026-11-12

## Context

speech.cpp (MIT) runs speech models on ggml: synthesis (Qwen3-TTS,
Irodori-TTS), recognition (Qwen3-ASR, NVIDIA's FastConformer models,
ReazonSpeech, Haas), detection, diarization and enhancement. It ships its own
OpenAI-compatible server, so it is also a competitor of Oaudio's. The
position Oaudio takes ([ADR 0001](0001-position.md), and the comparison in
`evidence.md`) is the layer above such engines: lifecycle, catalog and the
whole OpenAI audio API, with the engines doing the modelling. speech.cpp's
models are the test of that position, because its own server does not load
models on demand, serves only `wav` and `pcm`, and reads only WAV uploads.

The catalog had no model trained for Japanese. Whisper, SenseVoice and
Qwen3-ASR recognize it as one language of many.

## Decision

**A runner that starts `speech worker` and speaks its JSON Lines protocol.**
`runners/speechcpp` is pure Go and links nothing from speech.cpp:

- `speech worker MODEL` is a mode speech.cpp keeps for programs that embed
  it: one model per process, one JSON object per line on stdin and stdout,
  requests one at a time, and an exit when stdin closes. That is the runner
  contract's shape already, so the runner is a translation: PCM from the
  daemon goes out as base64 `chunk` lines followed by a `transcribe` line,
  and the `end` line's segments come back as protocol segments.
- The worker exits when its stdin closes, and the runner holds that pipe, so
  the worker cannot outlive the runner, even if the runner is killed. No
  supervisor is needed, unlike the audio.cpp server.
- The worker refuses detection models, and speech.cpp's documentation says
  its FastConformer recognizers drop whole sentences from a long stretch of
  speech. A recording longer than `whole_seconds` (30 s) is therefore cut
  into regions by `speech vad` with the Silero model shipped beside the
  recognizer, with the settings of speech.cpp's own `speech asr --vad`, and
  each region is recognized as one request.
- The runner checks the worker's protocol number (4) and refuses another,
  since speech.cpp raises it only for changes a caller must follow.

The first model is Haas-ja v1.0 (Apache-2.0), the most accurate Japanese
recognizer speech.cpp lists. Through Oaudio it returns the same text as
speech.cpp's own `speech asr` for all 650 recordings of FLEURS' Japanese test
set (`evidence.md`).

## Why not its server

- Its server fixes the models at start and serves them all from one process,
  which is the lifecycle Oaudio exists to provide. Running it per model would
  mean one HTTP server inside another for no gain.
- The worker is documented as a stable interface with a version number and a
  test client (`checks/smoke/worker_client.py`); the server's API is shaped
  for end users.

## Why not link it

speech.cpp has a C API (`speech.h`, major version 4), but the worker already
does the resampling, chunking and cancellation, and a separate process keeps
the runner buildable with `CGO_ENABLED=0` and upgradable by changing one tag.

## Consequences

- One loaded model is two processes: the runner and the worker. A long
  recording adds one short-lived `speech vad` process per request.
- Going through Oaudio adds about 35 ms per FLEURS recording on an M5 Pro
  (the HTTP upload, decoding, and the audio's base64 copy through the pipe),
  against 33 ms for the recognition itself in speech.cpp's own command line.
  Both are under 1% of the audio's length.
- Cancelling a request is passed to the worker, which stops at its next
  stopping point.
- The runner serves recognition and synthesis; see the addendum below. The
  worker's enhancement is not served.
- speech.cpp is built with `GGML_NATIVE=OFF`, so a release runs on any CPU of
  its architecture. Its own build defaults to tuning for the build machine.
- speech.cpp is young and has one main author, like audio.cpp. Nothing above
  the runner depends on it.

## Addendum, 2026-11-12: synthesis

The same runner serves the worker's synthesis models, with the same
process shape. What it adds:

- A `synthesize` request per call; the worker's `chunk` lines of PCM are
  passed to the daemon as they arrive, so Qwen3-TTS streams from its first
  frame. A cancel from the daemon becomes the worker's `cancel`.
- A custom voice reaches the worker as `add_voice` with the path of the
  reference recording the daemon already stores, once per recording, and is
  reused by name while the file is unchanged. Only models whose worker
  reports `voice_recordings` (Irodori-TTS) take one; the transcript is not
  needed.
- At load, the runner compares the manifest with the worker's `ready`: the
  task, the sample rate and every listed voice must match the model file, or
  the model does not load. A manifest error shows up at `oaudio run`, not as
  wrong audio.
- The daemon's splitting of input into sentences is kept, since Irodori-TTS
  makes a sentence whole before any audio and speech.cpp itself advises one
  sentence per request.

Two models were added: Qwen3-TTS 0.6B CustomVoice, the built-in-speaker
sibling of the cloning Qwen3-TTS that audio.cpp runs, and Irodori-TTS v4.1
Small MF, the first Japanese cloning model in the catalog.

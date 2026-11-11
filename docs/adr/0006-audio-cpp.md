# ADR 0006: audio.cpp is driven as a server, not linked

Status: accepted, 2026-11-11

## Context

The catalog had no model that clones a voice in Korean. Every permissive
candidate (`model-queue.md`) is a language-model-sized network that neither
whisper.cpp nor sherpa-onnx runs. Three engines could run them: audio.cpp
(C++ on ggml), mlx-audio (Python on MLX, Apple only) and llama.cpp's
`tools/tts` (command line only).

audio.cpp was tried first because one native binary covers Metal, CUDA,
Vulkan and CPU. Its README calls CUDA "the optimized path" and the others
"portability and testing", so the open question was whether it is usable on
a Mac at all. Measured on an M5 Pro with the Metal backend, at v0.9.4:
Qwen3-TTS 0.6B and Chatterbox Multilingual produce correct Korean in a
cloned voice faster than real time, and a speaker verifier matches their
clones to the right speaker 28 and 26 times in 30 (`evidence.md`). Ming 3.0
TTS also ran, but did not pass (`model-queue.md`).

## Decision

**A runner that starts `audiocpp_server` and talks to it over loopback
HTTP.** `runners/audiocpp` is pure Go and links nothing from audio.cpp:

- On load it writes a server configuration for the single model it was
  given, starts the server on a free port, and reports ready once the
  server's `/health` answers, which happens after the weights are in memory.
- `POST /v1/synthesize` becomes one `POST /v1/audio/speech` to the server.
  For families that stream, the server's raw PCM is passed through as it
  arrives; otherwise the WAV it returns is unwrapped.
- A cloned voice is passed as the path of its reference recording, which the
  daemon already keeps as a WAV file, plus its transcript when the model
  can use one.
- The server is started under a three-line shell supervisor holding a pipe
  from the runner. When the pipe closes, the server is stopped. The kernel
  closes it even if the runner is killed, so no server outlives its runner.

The manifest carries what differs between families: audio.cpp's family name,
the GGUF file, streaming or not, whether a reference transcript is required,
optional or unused, and any session options.

## Why not link it

- audio.cpp's C API exists, but its server is the interface its author tests
  and documents, and it already does reference loading, text chunking and
  streaming. Linking would mean redoing those through cgo.
- A second process costs a loopback hop on a request that takes seconds.
- The runner stays buildable with `CGO_ENABLED=0`, and audio.cpp can be
  upgraded by changing one tag in a Makefile.

## Consequences

- One loaded model is three processes: runner, supervisor shell, server.
- Stopping a request in progress closes the connection, but audio.cpp
  finishes generating before it notices. The next request waits for it.
- The runner is Unix only until the supervisor has a Windows equivalent.
- Only the model families listed in `runners/audiocpp/Makefile` are compiled
  into the server. Adding a model from another family means rebuilding.
- audio.cpp's own build script requires Xcode's Metal compiler. The Makefile
  calls cmake directly with the shaders embedded as source, which needs only
  the Command Line Tools.
- audio.cpp is young and has one main author. If it stalls, mlx-audio is the
  fallback on Apple hardware, and nothing above the runner would change.

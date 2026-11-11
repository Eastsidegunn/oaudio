# ADR 0003: Engines are separate processes behind a small protocol

Status: accepted, 2026-11-11

## Context

This was the riskiest assumption in the project. LLM runtimes converged on one
format and one engine, so Ollama could be a wrapper around a single library.
Speech has not converged: recognition and synthesis models come in at least
five unrelated architectures, run on different inference libraries (ggml,
ONNX Runtime, MLX, CoreML, PyTorch), and some of the best engines are
copyleft. If no single interface fits structurally different engines, "one
runtime for speech models" does not exist.

Two questions had to be answered: where does the boundary go, and how much
sits on each side of it?

Ollama's answer to the first, read from its source: one child process per
loaded model, started as `<exe> runner --model … --port …` and reached over
HTTP on loopback (`mlxrunner/client.go:439`, `llm/llama_server.go:457`).

## Decision

**One runner process per loaded model**, speaking protocol v1
(`docs/runner-protocol.md`, Go types in `runner/protocol.go`):

- The daemon starts the runner. The runner loads its model, then prints one
  JSON line with the loopback address it listens on. That line is the
  readiness signal; there is no port race.
- `POST /v1/synthesize` takes text and returns raw mono 16-bit PCM as it is
  produced. `POST /v1/transcribe` takes raw mono 16-bit PCM and returns
  segments as NDJSON as they are recognised.
- The runner exits when its stdin closes, so it cannot outlive the daemon.
  Unloading a model is ending its process, which returns all of its memory.

**The runner side is kept as thin as possible.** A runner only converts
between text and PCM. Everything else lives once, in the daemon: decoding
uploads, resampling, response encoding, sentence splitting, subtitles, joining
text, capability checks, scheduling, the public API.

Engines compiled into the `oaudio` binary (`apple`, and `reference` for tests)
run as `oaudio runner <engine>`. Any other engine is an executable named
`oaudio-runner-<engine>` found next to the binary, in `~/.oaudio/runners`, or
on `PATH`.

## What the evidence showed, and what changed because of it

The interface was exercised with engines that share nothing:

| Engine | Direction | How it works underneath |
|---|---|---|
| `apple` synthesis | text → PCM | spawns `/usr/bin/say`, reads the file it writes |
| `apple` recognition | PCM → segments | a Swift helper driving `SpeechAnalyzer`, streaming results |
| `reference` | both | pure Go, deterministic, for tests |

Two findings changed the design:

1. **Sentence splitting cannot be unconditional.** The daemon splits synthesis
   input into sentences so the first audio arrives after one sentence. With
   `say`, every call costs about 0.44 s before any audio exists, so splitting
   made a two-sentence input take twice as long (measured: 127 ms per sentence
   of average overhead over calling `say` directly). Runners now declare
   `whole_input` when splitting would only cost time; with it the overhead is
   7.5 ms per sentence. An engine's cost shape is part of its contract.
2. **Errors must keep their meaning across the boundary.** "Unknown voice" is
   the caller's mistake, not the engine's failure. Runners mark such errors
   (`runner.BadRequest`) and the daemon turns them into HTTP 400, which the
   OpenAI SDK raises as `BadRequestError`.

## Consequences

- Adding an engine means writing a runner, typically a few hundred lines, in
  any language. `internal/engines/ref/ref.go` is the smallest complete one.
- Copyleft engines (Piper is GPL-3.0) can be supported without the daemon
  linking them.
- A crash in an engine takes down one model, and the next request reloads it
  (`TestLoadFailureAndCrash`).
- Each request pays one extra HTTP hop on loopback. Measured cost end to end:
  3.4 ms per utterance for recognition, 7.5 ms per sentence for synthesis
  (`docs/evidence.md`).
- A runner serves one request at a time. Concurrency for one model means
  queueing, as in Ollama's default (`OLLAMA_NUM_PARALLEL=1`).

## The neural engines

The first version of this ADR had to say that the runners for whisper.cpp and
sherpa-onnx had never been compiled. They since have been, and the interface
held: both built on the first attempt and neither needed a change to the
protocol. What did need changing was inside the runners, found only by
running them:

- **whisper.cpp**: the binding's high-level API refuses to set a language on
  an English-only model, and silently switches to one segment per 30 seconds
  when given a callback. The runner now uses the low-level API, which
  streams properly segmented results.
- **sherpa-onnx**: unknown words are dropped rather than pronounced (ADR
  0005), and its shared libraries are found through a path into the build
  machine's module cache, which `runners/sherpa/bundle-libs.sh` replaces with
  a `lib/` directory beside the runner.

The protocol later grew three additions, all optional and none changing an
existing message: `POST /v1/vad`, `POST /v1/diarize`, and a `reference` voice
on synthesis requests (ADR 0005).

Engines now behind the one interface: whisper.cpp (ggml, Metal), sherpa-onnx
(ONNX Runtime) for four different model families, two macOS frameworks, and
the reference engine.

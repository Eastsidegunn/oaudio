# ADR 0001: What Oaudio is, and what it is not

Status: accepted, 2026-11-11

## Context

The starting idea was "Ollama, but for voice". Before writing code we read
Ollama itself rather than descriptions of it, to find out what position it
actually holds and whether that position is open for speech.

What Ollama is, read from its source at commit `f1ee5679` (2026-11-09):

| Fact | Where |
|---|---|
| A model is a `name:tag` that resolves to a manifest of content-addressed layers | `manifest/manifest.go`, `manifest/layer.go` |
| Each loaded model runs in its own child process, reached over loopback HTTP | `mlxrunner/client.go:439` (`exec.Command(exe, "runner", "--model", …, "--port", …)`), `llm/llama_server.go:457` |
| Models load on first use and unload after an idle period, 5 minutes by default | `envconfig/config.go:130`, `server/sched.go` (`refCount`, `expireTimer`) |
| The public surface imitates OpenAI's, so clients only change a base URL | `server/routes.go:2444-2446` |

The position, stated without reference to LLMs: turn a messy artifact into one
named unit, own the defaults so the first run needs no decisions, broker
scarce memory behind an always-on local daemon, and expose the endpoint that
existing clients already speak.

Is that position open for speech? Partly. **Ollama entered speech itself on
2026-10-24** (`ollama run phonon`, `POST /v1/audio/speech`), and its docs now
list `/v1/audio/transcriptions` and `/v1/audio/translations`. Read from the
same source tree and from `registry.ollama.ai`:

| What Ollama's speech support does | Where |
|---|---|
| Speech output is `wav` or `pcm` only; `mp3`, OpenAI's default, is rejected | `server/speech.go` (`format != "wav" && format != "pcm"`) |
| `speed` other than 1 and `instructions` are rejected | `server/speech.go` |
| Transcription offers `json`, `text`, `verbose_json`; no `srt` or `vtt` | `server/transcription.go:167` |
| Transcription runs through LLMs that accept audio; there is no dedicated recognition model class | docs: "models that accept audio input" |
| Speech models are repacked: every tensor is a separate `application/vnd.ollama.image.tensor` layer | `GET registry.ollama.ai/v2/library/phonon/manifests/latest` |
| The first speech model covers five European languages; no Korean | ollama.com/blog/speech-models |
| No voice activity detection, diarization, custom-voice or realtime route | `server/routes.go` has no `realtime` or `audio/voices` route; no Go file under `server`, `openai`, `api`, `mlxrunner` or `cmd` mentions VAD or diarization |

Demand that predates this and is still open in Ollama's tracker: Whisper
support (#1168, opened 2023-11-17, 64 upvotes, open), with comments asking for
whisper.cpp specifically, a WebSocket streaming interface, and an endpoint
compatible with existing clients.

## Decision

Oaudio takes the same *job* as Ollama, for developers who need speech as a
first-class thing rather than as a modality of a chat model:

1. **Dedicated speech models are the unit**, each run by the engine built for
   it (whisper.cpp for Whisper, sherpa-onnx for ONNX models, the operating
   system's own engines where they exist).
2. **Full OpenAI audio contract**, including the parts Ollama leaves out:
   `mp3`/`opus`/`aac`/`flac` output, `speed`, `srt`/`vtt`, word timestamps,
   streaming transcription.
3. **No repackaging.** Model files stay byte-identical to upstream, are
   verified against upstream's published hashes, and are usable by other tools
   (ADR 0002).
4. **Engines are separate processes behind a small documented protocol**
   (ADR 0003), so an engine can be added without touching the daemon and
   copyleft engines never link into it.
5. **License is first-class metadata**, shown before a developer ships.

6. **The rest of what a voice application needs from models**: voice
   activity detection, speaker diarization, voice cloning and realtime
   transcription. These were first listed as out of scope and were added the
   same day; ADR 0005 records why and how.

Explicitly **not** in scope: anything that needs a language model (realtime
*conversation*, voice-agent pipelines of VAD → STT → LLM → TTS), a GUI,
hosted inference, and a model registry of our own. Pipecat and LiveKit Agents
are consumers of a server like this, not things to rebuild.

## Consequences

- Oaudio competes with Ollama only at the edge where Ollama is generic. If
  Ollama adds dedicated recognition models and subtitle formats, the remaining
  difference is engine breadth, upstream-native storage and licensing clarity.
- "Whisper + one TTS behind pull/run/serve" alone would not have been a
  defensible project; that conclusion comes from the facts above, not from
  taste.

## Not verified

- Claims about other projects (Speaches, LocalAI, Lemonade, audio.cpp) in
  `docs/research.md` come from their READMEs and docs, not from running them.
  We did not install or time any competitor.

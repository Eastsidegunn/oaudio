# Landscape notes, November 2026

Background reading that shaped the project. **Read this as a map, not as
evidence.** Unless a line says "checked", it was gathered from READMEs, docs
pages and third-party articles on 2026-11-11 and has not been reproduced. The
facts the design actually rests on were re-read from primary sources and are
listed, with locations, in `adr/0001-position.md` and `evidence.md`.

## Ollama

- **Checked** (source at `f1ee5679`, registry, docs, blog): runner
  subprocesses, 5-minute keep-alive, one request per model by default
  (`envconfig/config.go:277`), speech output limited to `wav`/`pcm`,
  transcription without `srt`/`vtt`, per-tensor registry layers for its speech
  model, text-to-speech launch on 2026-10-24.
- **Checked** (GitHub API): 186.5k stars; issue #1168 "Support
  WhisperForConditionalGeneration" open since 2023-11-17 with 64 upvotes;
  #11798 audio input for multimodal models, open, 42 upvotes.
- Unchecked: it started as a packaging layer over llama.cpp and later added
  its own engine and an MLX runner; critics cite missing attribution in early
  releases, an opaque blob store, and templates that diverge from publishers'.
  Source: sleepingrobots.com/dreams/stop-using-ollama (2026-04-15), a single
  critical essay.

## Projects near the same position

All unchecked beyond reading their own pages.

| Project | What it is | Why it is not the same thing |
|---|---|---|
| Speaches | Python server, faster-whisper plus Kokoro/Piper, OpenAI-compatible including Realtime. Its README says it "aims to be Ollama, but for TTS/STT models". | Python and Docker packaging, CUDA-first, narrow model set. |
| LocalAI | One OpenAI-compatible server over roughly forty audio backends, with its own diarization and realtime routes. Functionally the most complete. | A general engine for every modality; speech is one corner of it. |
| Lemonade | C++ daemon with `pull`, Whisper and Kokoro. Closest in shape. | LLM-first and centred on AMD hardware. |
| sherpa-onnx | The broadest speech engine: recognition, synthesis, VAD, diarization, on everything down to microcontrollers. | An engine and SDK. No daemon, registry or lifecycle. Oaudio uses it. |
| whisper.cpp | Whisper on ggml, with a simple HTTP server. | One model family, one model per process. Oaudio uses it. |
| mlx-audio | Python library porting dozens of speech models to MLX. | Apple-only library, not a daemon. A natural future engine. |
| audio.cpp | Multi-family speech runtime on ggml with an OpenAI-style server, first released 2026-06-25. | Young, CUDA-first, Metal flagged as untested. A natural future engine. |
| Pipecat, LiveKit Agents | Voice-agent orchestration. | Consumers: LiveKit's docs point its OpenAI plugin at local servers by `base_url`. |

## Model and format fragmentation

Unchecked. The picture across sources:

- Recognition models fall into encoder-decoder (Whisper, Moonshine),
  CTC/transducer (Parakeet, SenseVoice) and audio-LLM (Qwen3-ASR, Voxtral)
  families. Synthesis splits into small non-autoregressive models (Kokoro,
  Piper), LLM-plus-codec models (Orpheus, Qwen3-TTS, Sesame CSM) and
  flow-matching models (F5-TTS). No engine runs all of them well.
- GGUF is spreading as a container for speech models, but each family still
  needs its own graph code, so it does not make engines interchangeable.
- Licenses are a minefield: XTTS-v2, F5-TTS weights, Fish and several newer
  models are non-commercial; Piper's engine is now GPL-3.0; pyannote and others
  are gated. This is why the manifest carries license fields.
- Korean coverage with commercial-use licenses is thin: Whisper and Qwen3-ASR
  for recognition; Qwen3-TTS and CosyVoice for synthesis. Kokoro and Parakeet
  do not cover Korean.

## What is missing everywhere

Unchecked, and the basis for the roadmap rather than for anything built:

- a shared way to name and move a *voice* between tools;
- standard endpoints for diarization, VAD and speaker enrolment;
- a simple documented streaming protocol short of the full Realtime API;
- published time-to-first-audio figures on Apple Silicon.

## Name

**Checked**: no package named `oaudio` on PyPI, npm or crates.io, and no Go
module at `github.com/oaudio/oaudio`. GitHub has a few small unrelated
repositories with the name, the largest a three-star Windows voice assistant.
No trademark search was done.

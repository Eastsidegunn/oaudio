# Engines

An engine is the software that actually runs a model. Oaudio starts each
engine as a separate runner process (`runner-protocol.md`).

## Status

| Engine | Models in the catalog | Platforms | State |
|---|---|---|---|
| `whisper.cpp` | `whisper:*` (recognition) | built and run on macOS arm64 with Metal | Working and measured. `runners/whisper`. |
| `sherpa-onnx` | `sensevoice`, `qwen3-asr`, `parakeet` and `moonshine` (recognition), `nemotron-asr` (streaming recognition), `kokoro` and `supertonic` (synthesis), `pocket-tts` (voice cloning), `silero-vad` (speech detection), `pyannote-diarize` (diarization) | built and run on macOS arm64, CPU | Working and measured. `runners/sherpa`. |
| `audio.cpp` | `qwen3-tts` and `chatterbox` (voice cloning, Korean included) | built and run on macOS arm64 with Metal | Working and measured. `runners/audiocpp`. |
| `speech.cpp` | `haas-ja` (Japanese recognition) | built and run on macOS arm64 with Metal | Working and measured. `runners/speechcpp`. |
| `apple` | `apple-say` (synthesis), `apple-speech` (recognition) | macOS; recognition needs macOS 26+ on Apple Silicon | Working and measured. Built into the `oaudio` binary. |
| `reference` | none | any | Test engine. Tones, not speech. |

None of the external runners has been built or run on Linux or Windows by hand; the release workflow builds them for Linux.

```sh
make            # oaudio and the macOS helper
make runners    # the whisper.cpp, sherpa-onnx, audio.cpp and speech.cpp runners; needs git, cmake, a C++ compiler
```

`oaudio catalog` shows which models have an engine installed.

## `whisper.cpp`

`runners/whisper/Makefile` clones whisper.cpp at a pinned tag (`v1.9.5`),
builds it as static libraries, and links the runner against them. The result
depends only on system libraries. On macOS it uses Metal.

- One GGML file per model, read from the path in the manifest.
- Decoding uses beam search with five beams, the default of whisper.cpp's own
  command-line tool. Against whisper.cpp's own server set to five beams, the
  transcripts were identical for 2,619 of 2,620 LibriSpeech recordings
  (`evidence.md`).
- Multilingual models detect the language when none is given. English-only
  models refuse other languages.
- `translate` is declared only on models trained for it (`large-v3-turbo` was
  not).
- Segments are sent to the client as whisper.cpp finalises them. Word times
  come from token timestamps and are approximate: they are good enough to
  assign words to speakers, not to cut audio on.
- A request for a client that has gone away is abandoned at the next
  30-second window.

## `sherpa-onnx`

`runners/sherpa/Makefile` builds the runner against the `sherpa-onnx-go`
module pinned in `go.mod` (`v2.0.1`), which carries prebuilt shared libraries
for sherpa-onnx and ONNX Runtime. `bundle-libs.sh` then copies those
libraries to `bin/lib/` and points the runner there, so `oaudio-runner-sherpa-onnx`
and `lib/` together can be moved to another Mac. Everything runs on the CPU.

The manifest's `family` selects the code path:

| Family | Task | Notes |
|---|---|---|
| `kokoro` | `tts` | 54 built-in voices; the manifest lists them in speaker-id order. `speed` supported. |
| `pocket` | `tts` | Voice cloning only: every request needs a custom voice. English. |
| `supertonic` | `tts` | Ten preset voices, 31 languages. Takes raw text, so it needs no dictionary and pronounces unfamiliar words. The language comes from the request, or is guessed from the script (Hangul, kana, Arabic, Devanagari, Greek); Latin-script text is taken as English unless told otherwise. |
| `sense-voice`, `transducer`, `qwen3-asr`, `dolphin`, `moonshine` | `stt` | sherpa-onnx's non-streaming recognizers, one code path for all. See below. |
| `online-transducer` | `stt` | sherpa-onnx's streaming recognizer: words while someone is still speaking. See below. |
| `silero` | `vad` | A fresh detector per request, since a detector carries state between windows. |
| `pyannote` | `diarize` | pyannote segmentation plus a speaker-embedding model and clustering. |

**Recognition.** Each of these families is one of sherpa-onnx's model
configurations; the manifest's `engine_config` names the files that go into
it. After loading they all behave the same way:

- They decode one utterance at a time and know nothing about segments. A
  recording of up to 30 seconds (20 for Qwen3-ASR, whose length limit covers
  audio and text together) is decoded whole and comes back as a single
  segment. A longer one is first cut at its silences by a Silero detector
  that the manifest ships next to the model (`engine_config.vad`), pieces
  less than a second apart are joined up to 30 seconds, and each piece
  becomes a segment with its own times.
- Word times are built from the engine's token times where the model gives
  them (SenseVoice, Parakeet). Qwen3-ASR gives none.
- Only SenseVoice takes a language hint. sherpa-onnx fixes that language when
  the recognizer is created, and changing it later through the engine's
  `SetConfig` also throws away the feature settings the model asked for, so
  transcripts silently change. The runner therefore loads a second copy of
  the model for a second language and keeps the two most recently used.
- `translate` is refused.

| Family | `engine_config` keys | Tried with |
|---|---|---|
| `sense-voice` | `model`, `tokens`, `language_hints` | SenseVoice-Small |
| `transducer` | `encoder`, `decoder`, `joiner`, `tokens`, `model_type` (`nemo_transducer` for NVIDIA's models) | Parakeet TDT 0.6B v3 |
| `qwen3-asr` | `conv_frontend`, `encoder`, `decoder`, `tokenizer` (a directory) | Qwen3-ASR 0.6B |
| `moonshine` | `encoder`, `merged_decoder`, `tokens` | Moonshine base and tiny, English |
| `dolphin` | `model`, `tokens` | Dolphin small (ran, not in the catalog) |

All of them also take `vad` (the Silero file for long recordings),
`whole_seconds` (the longest recording decoded in one piece, default 30) and
`max_speech` (the longest piece the detector may cut, default 20).

Two things found while measuring:

- SenseVoice writes Korean numbers inconsistently ("십 오미터", "20 십 일
  년"). That is the engine's output for this export, with or without Oaudio,
  and the runner does not correct it.
- A recording one sample short can come out as different words. The runner
  rounds when it converts a length in seconds back to samples; truncating
  lost a sample on some lengths and changed three Parakeet transcripts in
  2,620.

**Streaming recognition** (`online-transducer`, [ADR 0008](adr/0008-streaming-recognition.md)).
sherpa-onnx's `OnlineRecognizer` decodes audio in fixed chunks as it arrives
and never takes back a word, so the runner serves `POST /v1/stream` (realtime
sessions send it each new 200 ms of an utterance) as well as
`/v1/transcribe`, for which it feeds the whole recording through one stream
and reports a segment per finished sentence. Its keys: `encoder`,
`decoder`, `joiner`, `tokens`, `feature_dim` (128 for Nemotron, default
80), `language_option` (`1` when the model takes the language as a
per-stream option, as Nemotron 3.5 does; without a language it is told
`auto`), and `tail_seconds`, the silence appended at the end to flush the
model's look-ahead (default 0.8). Tried with Nemotron 3.5 ASR Streaming
0.6B, 560 ms chunks. Two findings:

- sherpa-onnx's assembled text for this model drops the spaces between
  Hangul syllables ("조국이당신을위해"), though the model emits them as
  tokens. The runner builds the text from the tokens instead.
- Told nothing, the model guesses the language from the first words, and
  can guess wrong: its own Japanese test recording began in Korean under
  `auto`. Clients should pass the language.

**Unknown words in Kokoro.** sherpa-onnx 2.x pronounces Kokoro text by
looking each word up in a dictionary and skips words that are not there,
with only a log line. The runner loads the same dictionary and rewrites
unknown words before synthesis: as the fewest dictionary words that spell
them (this project's former name, "Ovoice", became "o voice"), or letter by letter if no reasonable split
exists ("LibriSpeech" is spelled out). Nothing is dropped, but names will not
sound right. Proper pronunciation needs a grapheme-to-phoneme model, which
the engine can accept through an interface the runner does not use yet.

**Pocket TTS.** It needs no transcript of the reference recording. In
testing it twice left out a leading "A" ("A second sentence" came out as
"second sentence"), and a speaker verifier matched its clones to the right
person 8 times in 20 (`evidence.md`): expect a voice that resembles the
source, not a copy. Its diarization companion, `pyannote-diarize`, ships
with a clustering threshold of 0.8 instead of the engine's 0.5, chosen on
the test conversations described in `evidence.md`.

**Models that did not make it.** ZipVoice (zero-shot cloning, English and
Chinese) loads and runs in sherpa-onnx 2.0.1 but produces unintelligible
English, because its export expects eSpeak phonemes that the engine no
longer generates. It is not in the catalog.

## `audio.cpp`

`runners/audiocpp/Makefile` clones audio.cpp at a pinned tag (`v0.9.4`) and
builds its server, `audiocpp_server`, with only the model families the
catalog uses. On macOS it uses Metal. The runner itself is a small pure-Go
program that starts that server for one model and translates requests
([ADR 0006](adr/0006-audio-cpp.md)). Both files go next to `oaudio`; set
`OAUDIO_AUDIOCPP_SERVER` to use a server binary from somewhere else, and
`OAUDIO_AUDIOCPP_BACKEND` (`metal`, `cuda`, `vulkan`, `cpu`) to override the
backend.

The manifest's `engine_config` describes the model to audio.cpp:

| Key | Meaning |
|---|---|
| `family` | audio.cpp's family name, such as `qwen3_tts`. It must be one the server was built with. |
| `model` | The GGUF file. |
| `mode` | `streaming` if the family can stream audio as it is generated; otherwise the whole utterance arrives at once. |
| `reference_text` | What the model does with a transcript of the reference recording: `required`, `optional` or `never`. |
| `no_reference_text_options` | Request options to add when a cloned voice has no transcript. |
| `reference` | `optional` if the model can also speak with no reference recording. |
| `languages` | Languages the model accepts, to refuse the others before the engine does. |
| `session_options`, `request_options` | JSON objects passed through to audio.cpp. |

- **`qwen3-tts`** uses the transcript of the reference recording when the
  voice has one, and otherwise works from the speaker's timbre alone
  (`x_vector_only_mode`). In the Korean test the two scored the same
  (`evidence.md`).
- **`chatterbox`** uses its third multilingual checkpoint
  (`chatterbox.multilingual_t3=v3`), does not use a transcript, and does not
  stream. Resemble AI's model watermarks its output; we did not check
  whether audio.cpp's port does.
- A request that is cancelled keeps generating inside audio.cpp until it is
  done; the next request waits for it.
- audio.cpp's README describes Metal as a portability path and CUDA as the
  optimized one. On an M5 Pro both models ran faster than real time
  (`evidence.md`); slower Macs may not manage that.

## `speech.cpp`

`runners/speechcpp/Makefile` clones speech.cpp at a pinned tag (`v0.10.1`)
and builds its command, `speech`, installed as `speechcpp`; on macOS with
Metal, and with `GGML_NATIVE=OFF` so the binary runs on any CPU of its
architecture. The runner is a pure-Go program that starts `speechcpp worker`
for one model and speaks the worker's JSON Lines protocol
([ADR 0007](adr/0007-speech-cpp.md)). Set `OAUDIO_SPEECHCPP` to use a
`speech` binary from somewhere else.

| Key | Meaning |
|---|---|
| `model` | The GGUF file. |
| `languages` | Languages the model accepts, to refuse the others with 400 before the engine does. |
| `vad` | A Silero GGUF file. Recordings longer than `whole_seconds` are cut into regions of speech with it, by `speechcpp vad` with the settings of `speech asr --vad`, and each region is recognized on its own. Without it, every recording is recognized whole. |
| `whole_seconds` | The longest recording recognized in one request; default 30. |
| `options` | Extra flags for `speechcpp worker`, which become the defaults of every request (for example `--decoding greedy` for ReazonSpeech). |

- **`haas-ja`** is Third Intelligence's Haas-ja v1.0, a FastConformer with a
  TDT decoder trained for Japanese. It writes punctuation, and its segments
  are speech.cpp's sentence-level times. It neither translates nor detects
  the language; the language is Japanese.
- speech.cpp's documentation warns that its FastConformer recognizers drop
  whole sentences from a stretch holding several; that is why recordings
  over 30 s are recognized by region. A two-and-a-half-minute recording gave
  the same text through Oaudio as through `speech asr --vad silero-vad`;
  recognized whole, it lost its punctuation (`evidence.md`).
- The runner serves recognition only, so far. The worker also synthesizes and
  enhances speech.

## `apple`

Uses what ships with macOS, so there is nothing to download through Oaudio.

- **`apple-say`** runs `/usr/bin/say` with the system voices. `oaudio show
  apple-say --voices` lists the ones installed on this machine; more can be
  added in System Settings. A request may name a voice (`Yuna`, or `Eddy` for
  `Eddy (English (US))`) or only a `language` (`ko`), which picks the first
  voice that speaks it. `speed` maps to words per minute and is approximate.
  Use of the generated audio is governed by Apple's macOS license, which we
  have not analysed; the manifest marks the model non-commercial until
  someone has.
- **`apple-speech`** uses `SpeechAnalyzer` with `SpeechTranscriber`, on device.
  It does not detect the language: pass `language`, or it assumes `en-US`.
  The first use of a language makes macOS download its recognition model.
  It gives segment and word timestamps. It needs the helper binary
  `oaudio-apple-stt`, built by `make` from
  `internal/engines/apple/stt.swift` and expected next to `oaudio`.

These are the only models in the catalog that speak and understand Korean
without a download.

## Adding an engine

1. Write a runner to [`runner-protocol.md`](runner-protocol.md), in any
   language; `examples/runner.py` is a complete one in Python. In Go,
   implement whichever of `runner.Synthesizer`, `runner.Transcriber`,
   `runner.Detector` and `runner.Diarizer` apply, and call `runner.Main`.
   Check it with `go run ./tools/runner-check`.
2. Name the executable `oaudio-runner-<engine>` and put it next to `oaudio`,
   in `~/.oaudio/runners/`, in a directory listed in `OAUDIO_RUNNERS`, or on
   `PATH`.
3. Write a manifest with `"engine": "<engine>"` and drop it in
   `~/.oaudio/manifests/`. `oaudio manifest hf <repo>` fills in the file list
   and hashes.
4. Synthesize something and transcribe it back before believing it works. A
   model can load, run and return audio of a plausible length that says
   nothing.

Keep engines under copyleft licenses as separate executables. The daemon
talks to them over HTTP and links none of their code.

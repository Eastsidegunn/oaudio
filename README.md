# Oaudio

Run speech models locally, behind the API your code already speaks.

```sh
oaudio run whisper meeting.m4a --diarize        # who said what
oaudio run kokoro "Hello from Oaudio."          # text to speech
oaudio run qwen3-tts --voice mina "안녕하세요."    # in a voice cloned from a recording
oaudio run silero-vad call.wav                  # where the speech is
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:11535/v1", api_key="oaudio")

text = client.audio.transcriptions.create(model="whisper", file=open("a.wav", "rb")).text
speech = client.audio.speech.create(model="kokoro", voice="af_heart", input="Hello.")
```

Oaudio does for speech models what Ollama does for LLMs: one command to get
a model, one always-on local server that loads models on demand and unloads
them when idle, and an OpenAI-compatible API so existing clients only change
a base URL.

It covers recognition, synthesis, voice activity detection, speaker
diarization, voice cloning and realtime transcription.

> **Status: early, and tested on one machine.** Everything in this README
> runs on macOS on Apple Silicon. The engine runners have not been built on
> Linux or Windows, there are no releases, and CI has not run yet.
> [docs/evidence.md](docs/evidence.md) says exactly what was measured and
> what was not.

## Why not just use Ollama?

Ollama added text-to-speech on 2026-10-24, so the question is fair. Ollama
treats speech as one more thing a general model server can do. Oaudio is
built around dedicated speech models and the whole audio API. Each row was
read from Ollama's source, registry or docs; locations are in
[docs/adr/0001-position.md](docs/adr/0001-position.md).

| | Ollama (Nov 2026) | Oaudio |
|---|---|---|
| Recognition models | LLMs that accept audio | dedicated recognizers (Whisper, Parakeet, SenseVoice and others) |
| Speech output formats | `wav`, `pcm` | `mp3` (OpenAI's default), `opus`, `aac`, `flac`, `wav`, `pcm` |
| Subtitles, word timestamps | no | `srt`, `vtt`, word times |
| Who said what | no | `diarized_json` |
| Voice activity detection | no | yes |
| Custom voices | no | `/v1/audio/voices` |
| Realtime | no | `/v1/realtime` transcription sessions |
| Model files on disk | repacked, one registry layer per tensor | the publisher's files, verified against the publisher's hashes, usable by other tools |
| Adding an engine | in-tree | a separate program speaking a [documented protocol](docs/runner-protocol.md) |

If you already run Ollama and its speech support covers you, use it. Oaudio
runs next to it on a different port.

## Install

There are no releases yet. Build from source with Go 1.24 or later:

```sh
git clone https://github.com/eastsidegunn/oaudio && cd oaudio
make                # the server and CLI (pure Go, no dependencies)
make runners        # the whisper.cpp, sherpa-onnx and audio.cpp engines; needs git, cmake, a C++ compiler
./bin/oaudio run whisper recording.wav
```

On macOS, `make` alone is enough to try it: the speech engines built into
the system work with nothing to download (`oaudio run apple-say "Hello."`).

`oaudio` starts its server in the background the first time a command needs
it. To run it in the foreground: `oaudio serve`.

Optional: install `ffmpeg` to get `mp3`, `opus`, `aac` and `flac` output and
to accept uploads other than WAV.

## Models

`oaudio catalog` lists them with size, languages and license.

| Model | Does | Engine | Languages | License |
|---|---|---|---|---|
| `whisper` (five sizes) | recognition | whisper.cpp | multilingual, incl. Korean | MIT |
| `qwen3-asr` | recognition | sherpa-onnx | multilingual, incl. Korean | Apache-2.0 |
| `sensevoice` | recognition, small and very fast | sherpa-onnx | ko, en, ja, zh, yue | FunASR model license (commercial use allowed, with attribution) |
| `parakeet` | recognition, fast | sherpa-onnx | en and 24 other European languages | CC-BY-4.0 |
| `moonshine` (two sizes) | recognition, tiny | sherpa-onnx | en | MIT |
| `kokoro` | synthesis, 54 voices | sherpa-onnx | en, es, fr, hi, it, ja, pt, zh | Apache-2.0 |
| `supertonic` | synthesis, 10 voices | sherpa-onnx | 31 languages, incl. Korean | OpenRAIL-M (commercial use allowed, with use restrictions) |
| `qwen3-tts` | synthesis in a cloned voice | audio.cpp | 10 languages, incl. Korean | Apache-2.0 |
| `chatterbox` | synthesis in a cloned voice | audio.cpp | 19 languages, incl. Korean | MIT |
| `pocket-tts` (two sizes) | synthesis in a cloned voice, small, CPU only | sherpa-onnx | en | CC-BY-4.0, with a conflicting non-commercial note; see `oaudio show` |
| `silero-vad` | speech detection | sherpa-onnx | any | MIT |
| `pyannote-diarize` | who spoke when | sherpa-onnx | any | MIT |
| `apple-speech` | recognition | macOS | 30 locales, incl. Korean | Apple |
| `apple-say` | synthesis | macOS | the voices installed on the Mac, incl. Korean | Apple |

Candidates, and the models that were tried and left out, are in
[docs/model-queue.md](docs/model-queue.md).

Add your own by dropping a manifest in `~/.oaudio/manifests/`
([CONTRIBUTING.md](CONTRIBUTING.md)).

## Use

```sh
oaudio pull whisper                                # 547 MB, verified against upstream hashes
oaudio run whisper talk.mp3 --format srt           # subtitles
oaudio run whisper talk.mp3 --words                # JSON with word times
oaudio run whisper call.wav --diarize              # [00:04.1 - 00:10.0] B: I did. The numbers look better …

oaudio run kokoro --voice bm_george -o hi.mp3 "Good morning."
oaudio run supertonic --voice M2 "안녕하세요. 만나서 반갑습니다."     # Korean, language guessed from the text
echo "Piped text is spoken." | oaudio run kokoro
oaudio show kokoro --voices

oaudio voice create anna --audio anna.wav --transcribe-with whisper --consent "Anna agreed, 2026-11-11"
oaudio run qwen3-tts --voice anna "Spoken in a voice like Anna's."
oaudio run chatterbox --voice anna --language ko "안나의 목소리로 한국어를 말합니다."

oaudio ps                                          # what is in memory, and until when
oaudio stop whisper
```

Over HTTP, including realtime, see [docs/api.md](docs/api.md).

**Cloning a voice needs the consent of the person it belongs to.** Oaudio
asks who agreed and keeps the answer with the voice. It cannot check it.

## Measured

On an Apple M5 Pro, through the HTTP API. Scripts are in [`bench/`](bench);
method, raw results and limits are in [docs/evidence.md](docs/evidence.md).

| Check | Result |
|---|---|
| Official OpenAI Python SDK 3.49.0, unmodified | 22 / 22 checks pass, and its realtime client works too |
| Whisper base.en on all 2,620 LibriSpeech test-clean recordings: transcript through Oaudio vs. whisper.cpp's own server | identical for 2,619 (WER 3.93% both ways) |
| Whisper large-v3-turbo on all 382 FLEURS Korean test recordings: same comparison | identical for 379 (CER 3.25% both ways) |
| Parakeet, Moonshine and SenseVoice on the same LibriSpeech recordings, vs. sherpa-onnx's own package | identical for every recording up to 30 s; longer ones are cut at silences first |
| Diarization turns, Oaudio vs. the engine called directly, 30 conversations | identical for 30 |
| Time Oaudio adds to a request | 3 to 20 ms |
| English word error rate on LibriSpeech test-clean | Whisper turbo 1.8% (on a 500-recording sample), Parakeet 2.3%, Moonshine base 3.2%, SenseVoice 3.4% |
| Korean character error rate on FLEURS | Whisper turbo 3.3%, Qwen3-ASR 4.6%, SenseVoice 6.4% |
| Recognition speed, times faster than real time | SenseVoice 110 to 135 (CPU), Moonshine 80 to 120 (CPU), Parakeet 45 (CPU), Whisper turbo 25 to 40 (GPU) |
| Silences between speakers found by speech detection | 174 / 174 |
| Diarization error rate, 30 two-to-four-speaker conversations without overlap | 12.8%, threshold tuned on the same data |
| Transcript words attributed to the right speaker | 95% |
| Realtime: transcript after the speaker stops | 0.8 s median, 0.5 s of it the silence threshold |
| Korean text spoken by `supertonic` and transcribed back, 60 sentences | CER 3.1 to 3.3%, the recognizer's own error level on real speech |
| Korean voice clones identified as their source speaker by a verifier, of 30 | `qwen3-tts` 28, `chatterbox` 26 (real recordings: 10 of 10) |
| English voice clones identified as their source speaker, of 60 | `chatterbox` 57, `qwen3-tts` 53, `pocket-tts` 11 to 19 |

The first rows say that Oaudio returns what its engines return and adds
little. The rest is what the models manage, including where they are weak:
`pocket-tts` says the right words but does not sound much like the person,
SenseVoice is fast and less accurate, and diarization was only measured on
easy audio. Nobody listened to any of it; the judges were recognizers and a
speaker verifier.

## How it works

```
client ──HTTP / WebSocket──▶ oaudio serve ──loopback HTTP──▶ runner process (one per loaded model)
                             │  OpenAI-compatible API          │  whisper.cpp / sherpa-onnx / audio.cpp / macOS
                             │  decode, resample, encode       │  text ⇄ 16-bit PCM, spans, speaker turns
                             │  load on demand, keep-alive     │
                             └─ ~/.oaudio/models/<name>/<tag>/  upstream files, verified
```

- A **manifest** names a model, its engine, its upstream files with their
  published hashes, and its license. Oaudio never repackages weights.
  [ADR 0002](docs/adr/0002-manifest-and-store.md)
- A **runner** is a separate process per loaded model. Unloading a model
  ends its process. [ADR 0003](docs/adr/0003-runner-contract.md)
- The **scheduler** loads on first use, keeps a model warm for five minutes,
  and evicts the least recently used idle model when it needs room.
- The **API** is OpenAI's audio API in full, its realtime transcription
  sessions, and two endpoints of our own for speech detection and
  diarization. [ADR 0004](docs/adr/0004-api.md),
  [ADR 0005](docs/adr/0005-beyond-speech-and-text.md)

The server and CLI are pure Go with no dependencies outside the standard
library, including the WebSocket implementation.

## Known gaps

- macOS on Apple Silicon only, so far.
- Realtime transcription waits for a pause before it transcribes. No
  streaming recognizer is in the catalog yet
  ([docs/model-queue.md](docs/model-queue.md)).
- The cloning models need about 2 GB each and a GPU or Apple Silicon to keep
  up with speech. `pocket-tts` runs on a CPU but copies a voice poorly.
- Kokoro mispronounces words missing from its dictionary: they are respelled
  from known words or spelled out ([docs/engines.md](docs/engines.md)).
- Realtime sessions transcribe; they do not answer. Anything that needs a
  language model (conversation, voice agents) belongs to Pipecat, LiveKit
  Agents or similar, which can point at Oaudio.
- No GUI, no hosted inference, no model registry.

## Credit

Oaudio is plumbing. The models and the engines that run them are the work of
[whisper.cpp](https://github.com/ggml-org/whisper.cpp),
[sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx),
[audio.cpp](https://github.com/0xShug0/audio.cpp), OpenAI, Alibaba's Qwen,
FunAudioLLM and 3D-Speaker teams, NVIDIA, Moonshine AI, hexgrad, Supertone,
Resemble AI, Kyutai, Silero, pyannote, and Apple. The lifecycle
design follows [Ollama](https://github.com/ollama/ollama). See
[NOTICE](NOTICE).

## License

Apache-2.0. Models carry their own licenses; `oaudio show <model>` prints
them.

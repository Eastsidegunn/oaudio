# Model queue

Candidates for the catalog, and what happened to each. A model enters the
catalog only after it has been pulled, run, and measured here
(`evidence.md`); being listed below means nothing more than "worth a look".

**Status**

- `queued`: not yet tried.
- `next`: to be tried first.
- `in catalog`: verified and shipped.
- `rejected`: tried or examined, with the reason.

**Source of each row's facts**

- `seen`: we read the file listing, model card or source ourselves, or ran it.
- `reported`: from a research pass over third-party pages; not re-checked.

Gate for any synthesis or cloning model: synthesize public sentences,
transcribe them back, and compare. For recognition: a public test set
through Oaudio and directly through the engine. For licences: the original
model card, not the export's.

## Gaps this queue is meant to fill

1. Korean text-to-speech usable commercially: filled by Supertonic 3
2. Korean or multilingual voice cloning: filled by Qwen3-TTS and Chatterbox
3. Korean recognition smaller and faster than Whisper large-v3-turbo: filled
   by SenseVoice, at a cost in accuracy
4. Streaming recognition for realtime: filled by Nemotron 3.5 ASR Streaming
   (ADR 0008)
5. English recognition faster than Whisper: filled by Parakeet and Moonshine
6. Cloning that sounds more like the source than Pocket TTS: filled by
   Qwen3-TTS and Chatterbox
7. Extras: punctuation, wake word, denoising, language identification: open
8. Recognition trained for Japanese: filled by Haas-ja through speech.cpp

## On whisper.cpp and sherpa-onnx

Sizes are the compressed downloads. `reported` rows come from a research pass
over the engines' release pages and the original model cards on 2026-11-11.

### Verified or tried

| Model | Gap | Engine | Licence (weights) | Korean | Status | Notes |
|---|---|---|---|---|---|---|
| Whisper base / small / large-v3-turbo (GGML) | — | whisper.cpp | MIT | yes | in catalog | Identical to whisper.cpp's server on 2,619 of 2,620 recordings. |
| Kokoro-82M v1.0 | — | sherpa-onnx | Apache-2.0 | no | in catalog | Drops unknown words unless respelled. |
| Pocket TTS int8 and fp32 | 6 | sherpa-onnx | CC-BY-4.0; export README says non-commercial | no | in catalog | Weak cloning either way: a verifier matched clone to source 11 of 60 at 8 bits and 19 of 60 at full precision, against 53 to 57 for the audio.cpp models below. The 8-bit export is not the main cause. |
| Silero VAD v5 | — | sherpa-onnx | MIT | n/a | in catalog | |
| pyannote segmentation 3.0 + CAM++ zh_en | — | sherpa-onnx | MIT + Apache-2.0 | n/a | in catalog | DER 12.8% on easy audio, threshold tuned on the test set. |
| Supertonic 3 int8 | 1 | sherpa-onnx | OpenRAIL-M: commercial use allowed, with use restrictions to pass on | yes | in catalog | Korean round trip through Whisper turbo: CER 3.1 to 3.3% on 60 FLEURS sentences, about the recognizer's own error on real Korean speech. English WER 4.2%. Pronounces words Kokoro has to respell. Preset voices only. |
| SenseVoice-Small int8 (2024-07-17 export) | 3 | sherpa-onnx | FunASR Model Licence 1.1: free to use and share with attribution and the model's name kept; upstream states commercial use is permitted (seen) | yes | in catalog | FLEURS Korean CER 6.4% (Whisper turbo: 3.3%), LibriSpeech WER 3.4%, at 110 to 135 times real time on a CPU. Identical to the engine called directly on every recording up to 30 s. Writes Korean numbers inconsistently and spaces Korean loosely. |
| Qwen3-ASR 0.6B int8 | 3 | sherpa-onnx | Apache-2.0 (seen) | yes, detects language | in catalog | FLEURS Korean CER 4.6%, LibriSpeech WER 2.3%, 13 to 15 times real time on a CPU. Identical to the engine called directly on 382 of 382 and 499 of 500 recordings. Behind Whisper turbo on both sets at this size and precision, so it is in the catalog as an alternative, not as the default. Its reported strengths (dialects, noise, song) were not measured. |
| Parakeet TDT 0.6B v3 int8 | 5 | sherpa-onnx | CC-BY-4.0 (seen) | no; 25 European languages | in catalog | LibriSpeech WER 2.3% at 45 times real time on a CPU, with punctuation and capitals. Identical to the engine called directly on every recording up to 30 s. Returns nothing for Korean. |
| Moonshine base and tiny, English | 5 | sherpa-onnx | MIT, English models only (seen) | no | in catalog | LibriSpeech WER 3.2% (base, 106 MB) and 4.7% (tiny, 28 MB), at 80 and 120 times real time on a CPU. Identical to the engine called directly on every recording up to 30 s. |
| SenseVoice-Small int8, 2025-09-09 export | 3 | sherpa-onnx | unchecked | — | rejected | Not the original model: its README says it was converted from a Cantonese fine-tune (`ASLP-lab/WSYue-ASR`). The 2024-07-17 export is the original. |
| Dolphin small CTC int8 | 3 | sherpa-onnx | Apache-2.0 (seen) | yes | rejected | Ran it on FLEURS Korean: CER 7.7% against 6.4% for SenseVoice at a similar size, no punctuation, drops words in longer pieces, and returns nothing for English. |
| ZipVoice distill int8 | 2, 6 | sherpa-onnx | none stated | no | rejected | Ran it: unintelligible English, because it needs eSpeak phonemes the engine no longer generates. |
| CAM++ en VoxCeleb, ERes2Net base (for diarization) | — | sherpa-onnx | Apache-2.0 | n/a | rejected | Ran them: DER 42% and 19 to 36% against 12.8% for CAM++ zh_en. |

### To try, in order

| # | Model | Gap | Engine, API | Size | Licence (weights) | Korean | Status | Notes (`reported` unless marked) |
|---|---|---|---|---|---|---|---|---|
| 1 | Nemotron-3.5 streaming 0.6B (160 ms, 560 ms) | 4 | sherpa-onnx, `OnlineRecognizer` + language option | 453 MB each | NVIDIA OpenMDW-1.1, "ready for commercial use" | yes, CER 7.1 to 7.6% | next | Streaming recognition in 40 locales. Needs a streaming path in the runner protocol. Language switching through the Go API unproven. |
| 2 | Korean streaming Zipformer | 3, 4 | sherpa-onnx, `OnlineTransducerModelConfig` | 399 MB | Apache-2.0 tag; trained on KsponSpeech, whose terms are unchecked | Korean only, CER about 10 to 11 | queued | Lighter Korean-only fallback for streaming. An offline variant exists too (314 MB). |
| 3 | GTCRN denoiser | 7 | sherpa-onnx, denoiser API | 0.5 MB | MIT | n/a | queued | Needs a new task. |
| 4 | Whisper-tiny language identification | 7 | sherpa-onnx, `SpokenLanguageIdentification` | 111 MB | MIT | yes | queued | Would let a request pick a Korean or English model automatically. Needs a new task. |
| 5 | Cohere Transcribe 14-language int8 | 3 | sherpa-onnx | 1.6 GB | Apache-2.0 | yes, no auto-detect | queued | Large; accuracy pick. Needs a small addition to the runner's recognition path, which already covers five model types. |
| 6 | Qwen3-ASR 1.7B, and 0.6B at higher precision | 3 | audio.cpp, or sherpa-onnx if exported | — | Apache-2.0 | yes | queued | The 8-bit 0.6B export scored behind Whisper turbo on FLEURS Korean. Is that the size, or the 8 bits? |
| 7 | Parakeet TDT v3 via whisper.cpp `libparakeet` | 5 | whisper.cpp, C API `parakeet.h` | 356 MB to 2.5 GB | upstream CC-BY-4.0 (file repository is tagged MIT) | no | queued | No Go binding and no streaming; would need a small cgo adapter. sherpa-onnx already covers Parakeet, so low priority. |
| 8 | Pocket TTS Korean 300M (`seastar105`) | 2 | sherpa-onnx, if it can be exported | — | CC-BY-4.0 | yes, CER 6.1% | queued | The only Korean cloning lead on this engine, but it ships safetensors only: someone has to run the ONNX export, and compatibility is untested. |
| 9 | Kokoro int8, Kokoro v1.1 (zh) | — | sherpa-onnx, existing code | 132 MB | Apache-2.0 | no | queued | seen. Measure quality lost to 8 bits. |
| 10 | VoxLock target-speaker isolation; DPDFNet denoisers; punctuation; keyword spotting; audio tagging | 7 | sherpa-onnx | 0.5 to 69 MB | mostly Apache-2.0 or MIT, unchecked | no Korean models for punctuation or keywords | queued | Each needs a new task. |
| 11 | Silero VAD v6 (GGML) | — | whisper.cpp built-in VAD | 0.9 MB | MIT | n/a | queued | Would give Whisper its own speech filtering. |

### Ruled out before trying

| Model | Reason (`reported` unless marked) |
|---|---|
| VITS mimic3 Korean (KSS) | Ships eSpeak data, so it breaks in engine 2.x; the KSS dataset is CC BY-NC-SA. |
| Moonshine tiny-ko | Moonshine Community Licence: registration required, revenue capped at 1 million dollars. |
| Reverb diarization v1 / v2 | "Non-Production" licence: no commercial use. |
| TEN VAD | Apache-2.0 with added conditions: may not compete with Agora, and only for your own application and its direct users. Unsafe for a runtime that redistributes. |
| Kitten TTS, Matcha (en), Piper | English only and dependent on eSpeak. |
| MeloTTS | Only English and Chinese exports exist; no Korean export. |
| distil-whisper GGML | English only. |
| `whisper-large-v3-turbo-korean` GGML fine-tune | No size gain over turbo and weak evidence (WER about 16). |
| FireRedASR, X-ASR, FunASR-Nano | No Korean. |

## On audio.cpp

Added as an engine on 2026-11-11 ([ADR 0006](adr/0006-audio-cpp.md)). Rows
marked `seen` were run here; the rest are `reported` from the models' own
cards by a research pass on the same day, with the vendors' own figures.

### Verified or tried

| Model | Gap | Licence (weights) | Korean | Status | Notes (seen) |
|---|---|---|---|---|---|
| Qwen3-TTS 0.6B Base, 8-bit | 2, 6 | Apache-2.0 | yes | in catalog | Korean: a verifier matched clone to source 28 of 30, with or without a reference transcript. English: 53 of 60. Streams; first audio in about 0.2 s once the voice has been used. |
| Chatterbox Multilingual, 8-bit | 2, 6 | MIT | yes | in catalog | Korean: 26 of 30, and the lowest character error of the three. English: 57 of 60. Does not stream. Whether this port keeps the original's watermark was not checked. |
| Ming 3.0 TTS 0.5B, 8-bit | 1, 2 | MIT | yes | rejected for now | Speaks correct Korean in its own voice. As a cloner it failed both ways on audio.cpp 0.9.4: with a reference transcript most requests returned near-silence (character error 51%, 12 of 30 matched); from the recording alone the words were right but only 4 of 10 clones matched their speaker. Its vendor figure (similarity 0.79) was not reproduced. Worth another look on a later audio.cpp, or as a plain Korean voice. |

### To try

| Model | Gap | Licence (weights) | Korean | Size | Status | Notes (`reported`) |
|---|---|---|---|---|---|---|
| Qwen3-TTS 1.7B Base, CustomVoice, VoiceDesign | 1, 2, 6 | Apache-2.0 | yes | 1.7B | queued | Same family as the model in the catalog, so no rebuild. CustomVoice would add built-in voices that speak Korean. |
| CosyVoice3 0.5B | 1, 2 | Apache-2.0 | listed; no Korean figures on its card | 0.5B | queued | No streaming in audio.cpp. |
| VoxCPM2 | 1, 2, 6 | Apache-2.0 | CER 3.55 to 5.69, similarity about 0.83 | 2B, about 8 GB | queued | Slower than real time on an M4 Pro in one report. |
| MOSS-TTS-Nano | 1 | Apache-2.0 | listed for the larger models | 0.1B | queued | Small enough to be interesting on a CPU. |
| FireRedTTS3 | 1, 2 | Apache-2.0 (secondary source) | CER 1.64, similarity 0.86 in another vendor's table | unknown | queued | audio.cpp lists it with 24 languages; Korean unconfirmed. |
| MagpieTTS Multilingual 357M | 1 | unchecked | `ko` listed by audio.cpp | 357M | queued | NVIDIA; built-in speakers, no cloning. Seen in audio.cpp's model table, licence not read. |
| Confucius4-TTS | 2 | unchecked | `ko` listed by audio.cpp | — | queued | Marked experimental by audio.cpp. |
| Orpheus | 6 | Apache-2.0 plus Llama terms | no | 3B | queued | Not re-checked. |

### Ruled out on licence

| Model | Reason (`reported`) |
|---|---|
| OmniVoice | CC-BY-NC, because of its training data. |
| Fish S2 Pro | Research licence. Its Korean figure (CER 1.68) was the strongest seen. |
| Higgs Audio v3 | Non-commercial. |
| F5-TTS | CC-BY-NC. |
| IndexTTS-2 | bilibili licence with revenue limits; Korean not listed. |
| XTTS-v2 | CPML, non-commercial, and no licensor left to buy from. |

Not examined on primary pages yet: MeloTTS Korean, GPT-SoVITS, Zonos,
Spark-TTS, Dia2, F5 Korean fine-tunes. No 2026 open-weights synthesis model
from a Korean lab was found other than Supertone's Supertonic.

## On speech.cpp

Added as an engine on 2026-11-12 ([ADR 0007](adr/0007-speech-cpp.md)),
through its worker mode; recognition only so far. Its catalog is in its
`docs/models.md`; the figures below marked `reported` are its author's.

### Verified or tried

| Model | Gap | Licence (weights) | Status | Notes (seen) |
|---|---|---|---|---|
| Haas-ja v1.0, 8-bit | 8 | Apache-2.0 | in catalog | FLEURS Japanese CER 4.21%, against 7.67% for SenseVoice and 11.86% for Qwen3-ASR on the same set; identical to speech.cpp's own output on all 650. Writes punctuation. 0.66 GB. |

### To try

| Model | Gap | Licence (weights) | Notes (`reported`) |
|---|---|---|---|
| ReazonSpeech NeMo v2 | 8 | Apache-2.0 | Japanese with punctuation, beam search; CER 7.95% on FLEURS ja, so behind Haas. Takes recordings of many minutes whole. |
| Parakeet TDT-CTC 0.6B ja | 8 | CC-BY-4.0 | Japanese; CER 5.00% on FLEURS ja. |
| Qwen3-TTS 0.6B and 1.7B CustomVoice | 1 | Apache-2.0 | Nine built-in speakers including a Korean one (`sohee`), streaming. Needs synthesis in the runner. |
| Irodori-TTS | — | check | Japanese synthesis with cloning from a WAV file, 48 kHz. Needs synthesis in the runner. |
| Nemotron 3.5 Transcribe | 4 | NVIDIA licence, check | 30B (19 GB); speaker labels. Too large for most machines. |
| Sortformer (Nemotron 3 Diarization) | — | check | Streaming diarization, 106 MB. The worker does not serve diarization models, so it would need `speech diarize`. |

## Engines not adopted

| Engine | Would add | Why not now |
|---|---|---|
| **mlx-audio**, MIT, v0.6.5 on 2026-11-09 | About 27 synthesis families plus recognition, on the better-proven Apple path | Apple only, and a Python environment to ship. Kept as the fallback if audio.cpp stalls. `reported` |
| **llama.cpp `tools/tts`** | Qwen3-TTS, Pocket TTS, Chatterbox and a few more | Command line only, no synthesis server endpoint; audio.cpp covers the same models. `reported` |

## How the catalog is curated, and what that borrows from Ollama

How Ollama runs its library, from a research pass over its own pages on
2026-11-11 (`reported`; counts are as displayed that day):

- 269 official models, a median of 17 tags each. No published criteria and no
  submission process were found; model requests are GitHub issues (183 open,
  596 closed) that mostly get no reply.
- A bare name resolves to a small or mid-sized build at 4-bit quantization
  that fits consumer hardware, not to the flagship. Explicit size and
  quantization tags sit beside it.
- Headline models arrive on release day through launches coordinated with the
  publisher. Models without a partner wait on engine work, for weeks or
  indefinitely: the Whisper request has been open since 2023.
- The long tail is pushed to user namespaces and to `ollama run hf.co/...`,
  which is also what staff point requesters to.
- Licences are a layer on each tag page, with no badge or filter. A
  non-commercial variant can share a name with a permissive one.
- Known complaints: `deepseek-r1` resolving to a distilled model without
  saying so, lag behind upstream, a narrow set of quantizations.

What Oaudio takes from that:

| Practice | Oaudio |
|---|---|
| A bare name gives a default that fits common hardware | Yes. `whisper` is the quantized turbo model, not the largest. |
| Explicit tags beside the default | Yes: `whisper:base.en-q5_1`, `whisper:large-v3-turbo`. |
| A self-service path for everything else | Partly: user manifests in `~/.oaudio/manifests`. A `hf.co/...` shortcut is not built. |
| Publish what it takes to get in | Yes, unlike Ollama: the gate at the top of this file, and this queue with reasons for every rejection. |
| Licence visible before use | Yes, per model, with a commercial-use flag. A non-commercial variant never shares a name with a permissive one. |
| Honest names | A converted or distilled model is named for what it is and credits both the original authors and whoever converted it. |
| Release-day launches with publishers | No. That needs a partner's pre-release access. Oaudio follows releases; it does not lead them. |
| Own registry and per-family engine work | No. Files stay on their publishers' hosts, and engines are other projects' work behind adapters. |

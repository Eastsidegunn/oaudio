# Evidence

What has been checked by something other than the authors' opinion, how to
repeat it, and what has not been checked.

Everything below was measured on 2026-11-11 on one machine: Apple M5 Pro
(24 GB), macOS 26.5.1, Go 1.26.6, whisper.cpp v1.9.5 with Metal,
sherpa-onnx 2.0.1 on the CPU, audio.cpp v0.9.4 with Metal. Timings are
wall-clock, single run, on a machine doing other things; treat small
differences as noise.

The numbers describe the adapter, not model quality: Oaudio trains nothing.
They answer two questions. Does going through Oaudio return what the engine
returns? And what does each feature actually manage on data with known
answers?

## 1. Existing clients work unchanged

**Check:** the official OpenAI Python SDK, version 3.49.0, unmodified, with
only `base_url` pointed at Oaudio (`bench/sdk_conformance.py`).

| Engines behind the API | Result |
|---|---|
| Whisper, Kokoro, pyannote, Pocket TTS | 22 of 22 checks pass |
| Parakeet, Kokoro, pyannote, Qwen3-TTS | 22 of 22 checks pass |
| macOS engines | 18 of 18 checks pass (the four checks for diarized transcripts and custom voices need models those engines do not have) |

Covered: listing and retrieving models; speech with SDK defaults (the server
must answer with `mp3`); streaming speech to a file; `pcm` at 24 kHz; `opus`,
`aac`, `flac`; `speed`; transcription from an `mp3` upload; `text`, `srt`,
`vtt`, `verbose_json` with word and segment timestamps; streamed
transcription events; `diarized_json` decoded as the SDK's
`TranscriptionDiarized`; `client.audio.voices.create` and speech with
`voice={"id": …}`; and errors raised as the SDK's own `NotFoundError` and
`BadRequestError`.

**Realtime**, also with the unmodified SDK (`client.realtime.connect`,
`bench/realtime_sdk.py`): a recording streamed in 100 ms pieces at real-time
speed produced every expected event type, and the transcript of each
utterance arrived a median of 0.77 to 0.84 s after the speaker stopped, of
which 0.5 s is the configured silence threshold.

The same check on four Korean FLEURS recordings joined with pauses: all four
turns came back, a median of 0.96 s after the speaker stopped with
`sensevoice` and 1.2 s with `whisper`.

**Words while the speaker is still talking** ([ADR 0008](adr/0008-streaming-recognition.md)).
The same script, extended to time every delta, on two recordings made of
eight test recordings each joined with 1.5 s pauses (Korean: FLEURS, 125 s;
English: LibriSpeech test-clean, 80 s), streamed at real-time speed with
Silero VAD and 500 ms of silence ending a turn. Turn detection cut both at
pauses inside sentences too, into 12 and 11 turns, the same for every
model. Times are in the audio's own clock: "after onset" from the start of
the turn, "after the end" from the moment speech stopped
(`bench/results/realtime-stream-*.json`).

| | Korean, `nemotron-asr` | Korean, `sensevoice` | English, `nemotron-asr` | English, `parakeet` |
|---|---|---|---|---|
| Turns with words before the speaker stopped | 12 of 12 | 0 of 12 | 10 of 11 | 0 of 11 |
| First words, median, after onset | 1.41 s | 6.40 s | 1.09 s | 4.71 s |
| Share of the text that arrived while speaking | 88% | 0% | 89% | 0% |
| Interval between deltas while speaking, median | 0.60 s | — | 0.60 s | — |
| Final transcript, median, after the end | 0.77 s | 0.82 s | 0.74 s | 0.82 s |
| Deltas adding up to `.completed` exactly | 12 of 12 | 12 of 12 | 11 of 11 | 11 of 11 |
| CER over the whole recording | 14.8% | 10.0% | 0.8% | 0.6% |

The non-streaming models' first words come with their transcript, after
the turn has ended, so their "first words after onset" is the length of a
turn plus the final lag. The 0.6 s cadence is the model's 560 ms chunk.

The Korean CER of both models is higher here than on the FLEURS files
(7.78% and 6.44%), and the cause is the turn cutting, not the streaming:
the same turns cut out of the recording and sent as files to
`/v1/audio/transcriptions` gave `nemotron-asr` CER 14.8% again, with 11 of
the 12 transcripts identical to the realtime ones, and this recording
transcribed whole as one file gave 15.9%. Eight sentences is a small
sample; the full-set figures above are the ones to compare models by.

Not checked: the JavaScript SDK, Open WebUI, Pipecat, LiveKit Agents.

**Against another server.** `bench/compat_matrix.py` is a neutral version of
the check, written to be run against any server: every check stands alone,
errors are judged by exception type only, and recognition is fed a public
recording rather than the server's own speech. Oaudio passes 23 of 23;
audio.cpp's own server, release v0.9.4 with Supertonic and Whisper, passes 7
(`bench/results/compat/`). Other servers were not run.

## 2. Going through Oaudio returns what the engine returns

**Check:** send every recording of a public test set through the HTTP API
(upload → decode → resample → runner → engine) and also straight into the
engine with no Oaudio code involved; compare the transcripts and score both
with jiwer and the Whisper text normalizers (`bench/asr_eval.py`).

"Direct" is whisper.cpp's own HTTP server for Whisper, the engine's Python
package for sherpa-onnx, speech.cpp's own command line (`speech asr`, every
file in one process) for speech.cpp, and the Swift helper reading the
original file for the macOS recognizer.

| Engine and model | Test set | Recordings | Identical transcripts | WER | CER |
|---|---|---|---|---|---|
| whisper.cpp, `whisper:base.en-q5_1` | LibriSpeech test-clean, complete | 2,620 | 2,619 / 2,620 | 3.93% | 1.47% |
| whisper.cpp, `whisper:large-v3-turbo-q5_0` | LibriSpeech test-clean, 500 evenly spaced | 500 | 500 / 500 | 1.82% | 0.57% |
| whisper.cpp, `whisper:large-v3-turbo-q5_0` | FLEURS Korean test, complete | 382 | 379 / 382 | 13.03% | 3.25% |
| macOS, `apple-speech` | LibriSpeech test-clean, complete | 2,620 | 2,620 / 2,620 | 1.77% | 0.57% |
| macOS, `apple-speech` | FLEURS Korean test, complete | 382 | 382 / 382 | 14.25% | 3.40% |
| sherpa-onnx, `qwen3-asr` | FLEURS Korean test, complete | 382 | 382 / 382 | 14.64% | 4.55% |
| sherpa-onnx, `sensevoice` | FLEURS Korean test, complete | 382 | 382 / 382 | 22.48% | 6.44% |
| speech.cpp, `haas-ja` | FLEURS Japanese test, complete | 650 | 650 / 650 | — | 4.21% |
| sherpa-onnx, `nemotron-asr` (streaming) | FLEURS Korean test, complete | 382 | 382 / 382 | 27.44% | 7.78% |
| sherpa-onnx, `nemotron-asr` (streaming) | FLEURS Japanese test, complete | 650 | 650 / 650 | — | 15.47% |
| sherpa-onnx, `nemotron-asr` (streaming) | LibriSpeech test-clean, 500 evenly spaced | 500 | 500 / 500 | 3.46% | 1.22% |
| sherpa-onnx, `parakeet` | LibriSpeech test-clean, complete | 2,620 | 2,613 / 2,620 | 2.28% | 0.84% |
| sherpa-onnx, `qwen3-asr` | LibriSpeech test-clean, 500 evenly spaced | 500 | 499 / 500 | 2.27% | 0.79% |
| sherpa-onnx, `moonshine:base-en` | LibriSpeech test-clean, complete | 2,620 | 2,616 / 2,620 | 3.23% | 1.25% |
| sherpa-onnx, `sensevoice` | LibriSpeech test-clean, complete | 2,620 | 2,613 / 2,620 | 3.42% | 1.45% |
| sherpa-onnx, `moonshine:tiny-en` | LibriSpeech test-clean, complete | 2,620 | 2,614 / 2,620 | 4.71% | 2.05% |

| Engine and model | Check | Result |
|---|---|---|
| sherpa-onnx, `pyannote-diarize` | Speaker turns on 30 conversations, Oaudio vs. the engine's Python package | identical for 30 / 30 |

**The sherpa-onnx recognizers.** For these rows both paths were given the
same 16-bit WAV files (`--pcm16`), for the reason in the second point below.
Every recording of 30 seconds or less came back identical to the engine's
own Python package, for every model. The differing ones are all among the
eight LibriSpeech recordings longer than 30 seconds, which the runner cuts
at silences before decoding and the direct run decodes whole; that is a
choice the runner makes, not noise. Three things turned up on the way to
those numbers, and each would have gone unnoticed without the comparison:

- Changing SenseVoice's language through the engine's `SetConfig` silently
  reset its feature settings, and the same recording came back with other
  words and other number formatting. The runner now loads a recognizer per
  language instead. The first full Korean run, made with this and the next
  problem both present, matched on 168 of 382.
- FLEURS files hold 32-bit float samples. Oaudio and ffmpeg each round them
  to 16 bits, they disagree by one step on about one sample in a thousand,
  and that is enough to change a word. Dolphin, a model that was measured
  and not adopted, matched on 337 of 382 recordings with the float
  originals and on 382 with shared 16-bit files, with nothing else changed.
  Neither conversion is wrong. It does mean these models are not stable to
  the last bit of their input.
- Turning a length in seconds back into samples by truncation dropped the
  last sample of some recordings and changed three Parakeet transcripts in
  2,620. The runner now rounds.

**Japanese.** `haas-ja` was given FLEURS' 32-bit float files converted
once to 16-bit WAV, so both paths read the same samples; every one of the
650 transcripts matched speech.cpp's own. All 650 recordings are under 28 s,
so none was cut into regions. For the regions, 12 FLEURS recordings were
joined with a second of silence between them (148 s): through Oaudio the
text came back identical, character for character, to `speech asr --vad
silero-vad`. Recognized whole in one request instead, the same audio lost
all of its punctuation (17 marks by region, none whole) and scored worse,
CER 7.17% against 6.09% by region; no sentence was dropped on this
recording, though speech.cpp's documentation reports that happening on
others, which is the reason the runner cuts long recordings. The models already in
the catalog were run through Oaudio on the same 650 recordings for
comparison, without a direct run: SenseVoice CER 7.67%, Qwen3-ASR 11.86%.
Seven of Qwen3-ASR's 650 transcripts are a two-character Chinese word
(提纲 or 提言) instead of the sentence; that was not investigated further.
Japanese WER is not given: the text has no spaces between words.

**The streaming recognizer.** `nemotron-asr` (Nemotron 3.5 ASR Streaming
0.6B, 560 ms chunks) was given the same 16-bit files on both paths. Through
Oaudio the runner feeds each recording through one stream in 100 ms pieces
and reports a segment per sentence; the direct run gives sherpa-onnx's
Python package the whole recording at once. Both append the same 0.8 s of
silence before finishing and both build the text from the model's tokens
(sherpa-onnx's own text for this model drops the spaces between Hangul
syllables; `engines.md`). Every transcript matched, in all three sets, so
feeding a recording in pieces changes nothing. NVIDIA's model card gives
7.1 to 7.6% CER on FLEURS Korean for its own runs; 7.78% here is for the
8-bit export. It is less accurate than the non-streaming models on the
same sets (Korean: Whisper turbo 3.25%, Qwen3-ASR 4.55%, SenseVoice 6.44%;
English: Parakeet 2.28% WER) and well behind `haas-ja` in Japanese (4.21%
against 15.47%): it is in the catalog for realtime, where it is the only
model that can show words while someone is speaking (section 1). Time per
recording, Oaudio against direct: English −19 ms, Japanese +12 ms, Korean
+292 ms on the first full run and −21 ms when 100 of the same recordings
were run again; the first figure is not explained, and the direct run's
time is the engine's alone.

Read against Whisper large-v3-turbo on the same Korean set (CER 3.25%):
Qwen3-ASR at 8 bits is less accurate (4.55%) and SenseVoice less again
(6.44%) but several times faster than turbo. SenseVoice also writes
Korean numbers inconsistently (`engines.md`).

The four differing Whisper transcripts: one English recording of 1.5 s
(`260-123288-0014`, "Hans Sturrsnacht." against "Hon stirs nod."; the
reference is "Hans stirs not"), and three Korean recordings that differ in
one syllable or one space. The likely cause is that whisper.cpp's server
disables carrying text context between 30-second windows and the Oaudio
runner leaves the library default; this was not confirmed.

Limits:

- We found no figure published by the model authors for these exact
  quantized files on these sets, so the claim is "the same as the engine
  called directly", not "the same as the paper".
- Korean WER is high relative to CER mostly because of spacing and number
  formatting, which the basic normalizer does not reconcile. Do not compare
  these figures with numbers produced under another normalization.
- The LibriSpeech files are FLAC and go through ffmpeg; FLEURS files are
  16 kHz WAV and go through the native decoder. The resampler is covered
  separately by `TestResample`.

## 3. What the extra hop costs

Per request, from the same runs. "Direct" is the engine without Oaudio.

| Operation | Direct | Through Oaudio | Added |
|---|---|---|---|
| Whisper base.en, one LibriSpeech recording (7.4 s, FLAC upload) | 71.8 ms | 85.6 ms | 13.8 ms |
| Whisper turbo, one LibriSpeech recording (7.6 s, FLAC upload) | 300.8 ms | 308.4 ms | 7.6 ms |
| Whisper turbo, one FLEURS recording (12.6 s, WAV upload) | 337.9 ms | 311.0 ms | −26.9 ms |
| macOS recognizer, one FLEURS recording (WAV) | 106.8 ms | 110.2 ms | 3.4 ms |
| macOS recognizer, one LibriSpeech recording (FLAC) | 145.2 ms | 164.4 ms | 19.2 ms |
| macOS synthesis, one English sentence | 431.8 ms | 438.3 ms | 6.6 ms |
| Diarization, one 38 s conversation | 907.6 ms | 869.2 ms | −38.4 ms |
| speech.cpp `haas-ja`, one FLEURS recording (13.1 s, WAV) | 32.8 ms | 67.6 ms | 34.8 ms |

Read these as "a few to twenty milliseconds, the larger figure when an
upload must be decoded by starting ffmpeg". The negative rows are not Oaudio
being faster than its engine: whisper.cpp's server ran with its default of 4
threads against the runner's 8, and the diarization comparison is against a
Python wrapper. The direct Whisper runs also received pre-converted WAV
while Oaudio received FLAC.

Real-time factor through Oaudio (processing time ÷ audio duration): Whisper
base.en 0.012, Whisper turbo 0.025 to 0.041, macOS recognizer 0.009 to 0.022,
SenseVoice 0.007 to 0.009, Moonshine tiny 0.008 and base 0.012, Parakeet
0.022, Qwen3-ASR 0.065, Silero VAD 0.003, diarization 0.023, Kokoro 0.14,
Supertonic 0.18, Pocket TTS 0.19 to 0.23, Qwen3-TTS 0.43 to 0.56,
Chatterbox 0.58 to 0.65, macOS synthesis 0.05 to 0.11, `haas-ja` 0.005.

The `haas-ja` row is the largest added time in the table, though both
figures are under 1% of the audio. Its direct figure is one `speech asr`
process recognizing all 650 files (model load included, under a second),
so per recording it has no HTTP request at all; through Oaudio each
recording is also uploaded, decoded, and sent to the worker as base64 text
through a pipe. Which of those costs most was not measured.

For the sherpa-onnx recognizers, a request through Oaudio took within 8 ms
of the engine's Python package per recording, sometimes less than it
(SenseVoice 67.5 against 65.9 ms, Parakeet 165.8 against 173.8 ms, Moonshine
base 92.5 against 93.8 ms): at this size the difference between the two is
smaller than the difference between their wrappers.

## 4. Speech detection and diarization, against known answers

**Data:** 30 conversations built by `bench/make_mixes.py` from LibriSpeech
test-clean: 2 to 4 real speakers taking 6 to 8 turns, 0.6 s of silence
between turns, 18.8 minutes in all. Who spoke when is known exactly. There
is no overlapping speech, so this is an easy case and a floor, not a meeting
benchmark.

| Feature | Measure | Result |
|---|---|---|
| `silero-vad` | Inserted silences that fall outside every detected span | 174 / 174 |
| `pyannote-diarize` | Diarization error rate (0.25 s collar) | 12.8% (6.8% missed speech, 0% false alarm, 6.0% speaker confusion) |
| `pyannote-diarize` | Conversations with the right number of speakers | 20 / 30 |
| `whisper:base.en-q5_1` + `pyannote-diarize` | Transcript words attributed to the right speaker | 95.1% (transcript WER 4.2%) |

```sh
python bench/make_mixes.py --root LibriSpeech/test-clean --out mixes --count 30
uv run --with jiwer --with whisper-normalizer --with sherpa-onnx --with numpy \
    python bench/diar_eval.py --mixes mixes --stt whisper:base.en-q5_1 --direct \
    --segmentation <model.onnx> --embedding <embedding.onnx>
```

Limits, the first of which matters:

- **The clustering threshold (0.8) was chosen on these same 30
  conversations.** With the engine's own default of 0.5 the error rate was
  26.2% and the speaker count was right for 7 of 30. The reported 12.8% is
  therefore optimistic; it has not been checked on separate data.
- Much of the "missed speech" is pauses inside LibriSpeech utterances, which
  the reference counts as speech.
- Four speaker-embedding models were compared on this set; the one in the
  catalog was the best of them, by a wide margin.

## 5. Voice cloning, against real speakers

**Check** (`bench/clone_eval.py`): one recording of a speaker becomes a
custom voice, and the model speaks three sentences that speaker never said.
A speaker-verification model not used anywhere inside Oaudio (WeSpeaker
ResNet34) then compares each clone with a *different* recording of the same
speaker and with every other speaker in the cast. The clones are also
transcribed and scored against the text, next to the same recognizer's score
on the real recordings.

**Korean**: the 10 speakers of the Zeroth-Korean test set, 30 clones per
model, transcribed by Whisper large-v3-turbo.

| Model | Clone matched to its source speaker | Similarity to source / to others | CER of cloned speech | Real-time factor |
|---|---|---|---|---|
| Real recordings, for scale | 10 / 10 | 0.891 / 0.577 | 5.1% | |
| `qwen3-tts`, voice with transcript | 28 / 30 | 0.785 / 0.496 | 7.8% | 0.52 |
| `qwen3-tts`, voice without transcript | 28 / 30 | 0.779 / 0.505 | 6.7% | 0.43 |
| `chatterbox` | 26 / 30 | 0.808 / 0.513 | 4.9% | 0.65 |

**English**: 20 LibriSpeech test-clean speakers, 60 clones per model,
transcribed by Whisper base.en.

| Model | Clone matched to its source speaker | Similarity to source / to others | WER of cloned speech | Real-time factor |
|---|---|---|---|---|
| Real recordings, for scale | 19 / 20 | 0.859 / 0.585 | 4.2% | |
| `chatterbox` | 57 / 60 | 0.790 / 0.530 | 2.2% | 0.58 |
| `qwen3-tts` | 53 / 60 | 0.782 / 0.536 | 2.1% | 0.56 |
| `pocket-tts:fp32` | 19 / 60 | 0.676 / 0.537 | 5.0% | 0.19 |
| `pocket-tts:int8` | 11 / 60 | 0.661 / 0.541 | 5.7% | 0.23 |

Time to the first audio byte for a three-sentence Korean request, with the
model loaded and the voice used once before: `qwen3-tts` 0.21 to 0.23 s (it
streams), `chatterbox` 3.9 to 4.4 s (it returns the whole utterance).

**Reading:**

- The two audio.cpp models copy a voice well enough that a verifier picks
  the right speaker about nine times in ten, in both languages, and their
  speech is as intelligible to the recognizer as the real recordings.
- The Korean character error rates look high because Zeroth-Korean spells
  numbers out ("천 구백 육십 년대") and the recognizer writes digits. The
  real recordings score 5.1% for the same reason. Read the clones against
  that row, not against zero.
- Giving `qwen3-tts` the transcript of the reference made no measurable
  difference here.
- Pocket TTS says the right words but does not sound much like the source.
  Full precision helps a little (19 against 11 of 60), so the 8-bit export
  is not the main cause. It was not compared with Pocket TTS run outside
  sherpa-onnx. It was also seen to drop a leading "A" twice.
- The sampling in `qwen3-tts` and `chatterbox` is random. A first Korean
  run of 10 clones each gave 10 / 10 and 8 / 10; the tables are the larger
  runs. Expect a few clones either way on a repeat.
- The verifier was trained on English-language celebrity recordings. It
  separates the Korean speakers cleanly (10 / 10 on real recordings), but
  its scores are not calibrated for Korean.
- Nobody listened to the results.

**Ming 3.0 TTS 0.5B did not pass** and is not in the catalog. Given the
reference transcript, most requests returned near-silence (CER 51%, 12 of 30
matched). Given only the recording, the words were right (CER 4.6%) but 4 of
10 clones matched their speaker. Both result files are in `bench/results/`.
This is audio.cpp 0.9.4 on Metal with the 8-bit package; the original model
was not run.

The voices were removed after scoring and no cloned audio was kept.

## 6. Speech comes out, and it says the right words

Synthesize public sentences through the API, transcribe the audio through
the API, compare with the input (`bench/tts_eval.py`). Round-trip error
includes the recognizer's own errors.

| Synthesis | Recognizer | Sentences | Round-trip WER | CER |
|---|---|---|---|---|
| `kokoro`, voice `af_heart`, English | `whisper:base.en-q5_1` | 60 | 9.3% | 1.6% |
| `supertonic`, voice F1, Korean | `whisper:large-v3-turbo-q5_0` | 60 (FLEURS) | 10.9% | 3.3% |
| `supertonic`, voice M2, Korean | `whisper:large-v3-turbo-q5_0` | 60 (FLEURS) | 10.5% | 3.1% |
| `supertonic`, voice F1, English | `whisper:large-v3-turbo-q5_0` | 60 (FLEURS) | 4.2% | 2.3% |
| `apple-say`, voice Samantha, English | `apple-speech` | 60 | 2.8% | 0.8% |
| `apple-say`, voice Yuna, Korean | `apple-speech` | 60 | 14.3% | 5.6% |

For scale: the same recognizer scores 3.25% CER on real Korean speech
(section 2), so Supertonic's Korean is about as intelligible to it as a
person's. Most mismatches are punctuation and number formatting ("두 개"
heard as "2개").

Kokoro's engine logged no skipped words in this run. Before the runner's
workaround (ADR 0005), words missing from its dictionary were silently left
out of the audio.

Median time to the first audio byte: Kokoro 0.57 s, macOS voices 0.44 s,
Supertonic 1.5 s (real-time factor 0.18).

## 7. Downloads are the publisher's bytes

`oaudio pull whisper:base.en-q5_1`: the file on disk hashes to
`4baf70dd0d7c4247ba2b81fafd9c01005ac77c2f9ef064e00dcf195d0e2fdd2f`, equal to
`lfs.oid` for `ggml-base.en-q5_1.bin` from the Hugging Face API. The Kokoro
(19 files from one repository), diarization (files from two repositories)
and Pocket TTS (a `tar.bz2` from a release page, pinned by the SHA-256 the
release API reports) downloads all completed and verified.

Tests against a fake host cover resuming with a Range request, rejecting and
deleting content whose hash does not match, a missing file, modification
after install, unpacking an archive, and refusing archive entries that are
links or would land outside the model directory
(`internal/pull/pull_test.go`).

Not checked: a gated model with `HF_TOKEN`.

## 8. The lifecycle and the protocol behave

`go test -race ./...`, all passing: one load for concurrent first requests;
one request at a time per runner; keep-alive expiry, `0` and "forever";
least-recently-used eviction; waiting when every model is busy; a failed
load; a crashed runner dropped and reloaded; stop; shutdown; the WebSocket
handshake, fragmentation, ping/pong, close, and five kinds of protocol
violation; a realtime session end to end, and one with a streaming model
(deltas before `speech_stopped`, adding up to the transcript); streams
started, grown, ended, abandoned and refused (404 for an unknown stream);
resampling a stream piece by piece giving exactly the samples of resampling
it whole; custom voices; speaker assignment.

Against real processes (`runner/runner_test.go`, `scripts/e2e.sh`): the
readiness handshake, stopping a runner ends its process, a runner that fails
or hangs while loading is reported and killed, and stopping the server
leaves no runner behind.

## 9. A release installs and runs on its own

**Check:** `scripts/package.sh` built the macOS archive (29 MB, every engine
and its libraries). `scripts/install.sh` installed it from a local mirror into
an empty home directory, with `PATH` cut down to `/usr/bin:/bin`: no Go,
Homebrew, ffmpeg, compiler or source tree. From there, models were pulled
fresh and each engine ran: whisper.cpp and SenseVoice transcribed an English
sentence exactly, Supertonic spoke Korean (Apple's recognizer read it back
exactly), and Qwen3-TTS spoke in a voice cloned from a recording. Every binary
in the archive declares macOS 14 as its minimum (`otool`), the oldest
release a Mac with the Metal backend needs.

Not checked: running on macOS 14 or 15 (only 26 was available), and the
Linux archives, which only the release workflow builds and installs.

## Not verified

- **Speed and quality against other runtimes.** Only API compatibility was
  compared (section 1). The comparison with Ollama in the README is about
  features read from its source.
- **One machine, one operating system.** The runners have not been built or
  run on Linux or Windows. Linux and Windows builds of the server compile and
  were not run.
- **CI has never run**, neither `ci.yml` nor `release.yml`.
- **Streaming against OpenAI's own service.** The event order with deltas
  before `speech_stopped` follows the SDK's types and documentation; no
  OpenAI realtime session was recorded to compare against.
- **Other chunk sizes of Nemotron** (80 to 1120 ms), the trade between first
  words and accuracy they give, and any streaming model but Nemotron.
- **Streaming with languages other than Korean, English and Japanese**, or
  with `auto`, which misheard the model's own Japanese test recording.
- **Many streaming sessions at once** on one runner: each piece is a short
  request, so they interleave, but nothing measured how many keep up.
- **An install on a second, clean Mac.** Section 9 isolated the install on
  the development machine; system libraries and frameworks were still that
  machine's.
- **Memory use and cold-start time** were not measured systematically.
- **Diarization and VAD on hard audio**: overlap, noise, telephone or
  far-field speech, languages other than English.
- **Audio quality** was judged only by recognizers and a speaker verifier,
  never by a listener.
- **GPU or CoreML acceleration for the sherpa-onnx models.** They ran on the
  CPU.
- **audio.cpp anywhere but Metal.** Its CUDA, Vulkan and CPU backends were
  not run through Oaudio, and neither was a Mac slower than an M5 Pro, where
  the cloning models may fall behind real time.
- **Whether Chatterbox's watermark survives** in audio.cpp's port.
- **speech.cpp beyond one model on Metal.** Only `haas-ja` was run, on an M5
  Pro. Its CPU and Vulkan backends, its other recognizers (ReazonSpeech,
  Parakeet ja) and its synthesis models were not run through Oaudio. The
  `GGML_NATIVE=OFF` build was not run on a different CPU.
- **Japanese beyond FLEURS**: conversational, noisy or long-form speech, and
  any listener's judgement of the punctuation.
- **Cloning across languages** (a Korean voice speaking English, or the
  reverse), and reference recordings that are short, noisy or not read
  speech.

## Data

- LibriSpeech test-clean: Panayotov et al., CC BY 4.0, openslr.org/12.
  Archive SHA-256 `39fde525e59672dc6d1551919b1478f724438a95aa55f874b576be21967e6c23`.
- FLEURS: Conneau et al., CC BY 4.0, huggingface.co/datasets/google/fleurs
  (`ko_kr`, test split; `ja_jp`, test split, read at revision `70bb2e84`).
- Zeroth-Korean test set: Jo and Lee, CC BY 4.0, openslr.org/40, read from
  huggingface.co/datasets/kresnik/zeroth_korean at revision `1fe93789`
  (457 recordings, 10 speakers).

The result files in `bench/results/` contain reference transcripts from these
sets under those licenses.

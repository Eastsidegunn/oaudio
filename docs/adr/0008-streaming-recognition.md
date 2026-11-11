# ADR 0008: Words arrive while the speaker is still talking

Status: accepted, 2026-11-12

## Context

Oaudio's realtime endpoint (`/v1/realtime`, OpenAI's transcription
sessions) found where each utterance ended and only then transcribed it. A
client saw nothing while someone spoke and the whole transcript about 0.8 s
after they stopped. For captions, dictation and voice agents, which are what
the realtime API is for, words that appear as they are said matter more than
that last 0.8 s.

The OpenAI API already describes this. In the SDK's types (openai 3.49.0),
`conversation.item.input_audio_transcription.delta` is "returned when the
text value of an input audio transcription content part is updated with
incremental transcription results", carrying an `item_id` and a `delta`; the
transcription settings have a `delay` ("how long the model waits before
emitting transcription text", for `gpt-realtime-whisper`); and the
translation API's transcript deltas are documented as "append-only text
fragments". So a client written for OpenAI already shows deltas as they
come. Nothing new is needed in the public API, only something to send.

Recognizers built for this exist. A cache-aware streaming transducer decodes
audio in fixed chunks as it arrives and never revises a word it has emitted.
NVIDIA's Nemotron 3.5 ASR Streaming (0.6B, OpenMDW-1.1) is one, with 35
languages including Korean, Japanese and English, and sherpa-onnx runs its
published ONNX export through its `OnlineRecognizer`.

The runner protocol had no way to send audio a piece at a time: every
recognition was one request carrying a whole recording.

## Decision

**A streaming session in the runner protocol, driven by the daemon's turn
detection.**

### Runner protocol

Added within version 1, so no runner has to change:

- A manifest capability, `stream`. The daemon streams only to models that
  declare it.
- `POST /v1/stream?session=ID&sample_rate=R[&language=L][&start=1][&end=1]`,
  the body the next piece of the utterance as PCM. The answer is
  `{"text": "...", "done": false}` with the **whole transcript so far**, not
  the change: a runner need not remember what it already reported, and the
  daemon, which does, computes the deltas.
- `start=1` begins a stream; `end=1` finishes it (`"done": true`, with the
  final transcript) and the runner forgets it. A piece for a session the
  runner does not know is answered **404**: after a runner restart or an
  idle drop the earlier audio is gone, and a transcript of the rest alone
  would look correct and be wrong.
- `DELETE /v1/stream?session=ID` abandons a stream.
- The one-request-at-a-time rule is unchanged. Each piece is its own short
  request, so a runner keeps several streams between requests and other
  requests can run between pieces.
- A runner that streams still answers `/v1/transcribe` for whole
  recordings. The Nemotron runner feeds the recording through one stream and
  emits a segment per finished sentence.

HTTP request-response per piece was chosen over a duplex stream (a chunked
request body answered while it is still being sent) because it is what every
HTTP library does by default, including Python's standard library, which the
example runner uses; and a piece every 200 ms costs a loopback round trip of
well under a millisecond.

### Daemon

- Turn detection is unchanged: Silero VAD (or loudness) still decides when
  an utterance starts and ends, and `speech_started` / `speech_stopped` mean
  what they meant.
- From `speech_started` on, every detection tick (200 ms) sends the audio
  that arrived since the last one. Each time the transcript grows, the
  growth goes out at once as a `delta` for that item, before
  `speech_stopped` if the speaker is still talking.
- At `speech_stopped` the rest is sent with `end=1`, and the final
  transcript goes out as `.completed`. Items keep their events in order:
  `speech_started`, deltas, `speech_stopped`, `committed`, `item.added`, any
  last deltas, `.completed`.
- Deltas only append. If a model ever revised words it had reported, the
  daemon would send nothing until its transcript grew past them again;
  `.completed` always carries the final transcript whole.
- Audio is resampled from the API's 24 kHz to the model's rate piece by
  piece without changing a sample: only the output samples whose filter
  window lies wholly in the audio so far are sent, and the rest wait for the
  next piece or the end (`audio.ResampleStable`). The runner receives exactly
  what resampling the whole utterance at once would have given it.
- With turn detection off (`turn_detection: null`), the client commits the
  utterance and it is transcribed whole, as before; deltas while speaking
  need the server to know that someone is speaking.
- Models without `stream` keep the original path.

## Consequences

- Words in about a second instead of after the utterance: with Nemotron's
  560 ms chunks, the first words of a turn reach the client while it is
  being spoken, and the final transcript follows the end of speech by about
  as long as before (`evidence.md`).
- One more endpoint for runner authors, optional, checked by
  `tools/runner-check` when the manifest declares `stream`.
- The chunk size, and so the trade between latency and accuracy, is fixed
  by the exported model: NVIDIA's model takes 80 ms to 1120 ms chunks, and
  sherpa-onnx publishes one export per size. The catalog carries 560 ms.
- A model that only streams in this sense, with no turn detection of its
  own, still relies on the daemon's speech detector to end turns.
- `/v1/audio/transcriptions` with `stream=true` is unchanged: it already
  sends a delta per segment as a recording is recognized, and a streaming
  model's segments are its sentences.
- sherpa-onnx's own text for this model drops the spaces between Hangul
  syllables, though the model emits them as tokens; the runner joins the
  tokens instead. The direct comparison in `bench/asr_eval.py` does the same.

## Not chosen

- **Re-transcribing the growing utterance with an offline model** every
  tick (what some servers do to show interim text). Its interim text is
  revised, which OpenAI's append-only deltas cannot express, and its cost
  grows with the utterance.
- **Letting the model end turns** with its own endpoint detection. Silero
  VAD is already measured on the daemon's path and works with every model.

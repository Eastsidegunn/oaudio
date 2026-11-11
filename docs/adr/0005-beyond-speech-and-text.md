# ADR 0005: Speech detection, diarization, voice cloning and realtime

Status: accepted, 2026-11-11. Extends ADR 0001, whose first version listed
all four as out of scope.

## Context

ADR 0001 placed Oaudio where Ollama is generic about speech. The four things
this ADR adds are exactly what Ollama's speech support does not have (read
from its source and docs, see ADR 0001): no realtime endpoint, no voice
cloning, no voice activity detection, no diarization. They are also what a
voice application needs after "transcribe" and "speak".

The question for each was the same as in ADR 0004: is there an existing
contract that clients already speak? The source for that was the official
OpenAI Python SDK, version 3.49.0, read from its installed files
(`openai/types/audio/`, `openai/types/realtime/`), since the published
OpenAPI document we first downloaded predates these features.

| Need | Existing contract found in the SDK |
|---|---|
| Who said what | `response_format="diarized_json"` on transcriptions: segments of type `transcript.text.segment` with `speaker` labelled `A`, `B`, … |
| Speak in a custom voice | `POST /v1/audio/voices` with `name`, `audio_sample`, `consent`; then `voice={"id": …}` on speech requests |
| Live transcription | Realtime API transcription sessions over WebSocket: `session.update`, `input_audio_buffer.append`, server VAD, `conversation.item.input_audio_transcription.completed` |
| Where the speech is | none |

## Decision

**Two new tasks**, alongside `stt` and `tts`: `vad` (which stretches of audio
contain speech) and `diarize` (who spoke when). Each is a model with its own
manifest, runner endpoint and lifecycle, like any other.

**Follow the existing contract wherever there is one; extend in the same
style where there is not.**

- `diarized_json` on `/v1/audio/transcriptions`. Transcription and
  diarization are separate models, so the diarizer is named in a
  `diarization_model` field, or implied when exactly one is installed. When
  the transcription model gives word times, each word goes to whoever is
  speaking when it starts, so a transcript segment that spans a change of
  speaker is split at the change. `known_speaker_names` is refused.
- `POST /v1/audio/voices` as OpenAI defines it, plus list and delete. A voice
  is a short reference recording stored once and usable by any model that
  declares `voice_clone`. A model without it refuses a custom voice rather
  than quietly speaking in another.
- `GET /v1/realtime` implements transcription sessions. Conversation
  sessions, where a model also answers, need a language model and stay out
  of scope; asking for one gets an error event that says so.
- `POST /v1/audio/vad` and `POST /v1/audio/diarizations` are Oaudio's own,
  shaped like the other audio endpoints (multipart upload, `model`, JSON
  out), because nothing standard exists.

**Consent is recorded, not verified.** OpenAI's `consent` field is the id of
a consent recording it has checked. A local runtime cannot check anything, so
`consent` is a required free-text statement of who agreed, stored with the
voice and shown by `oaudio voice list`. This makes the question unavoidable
and leaves a record; it does not stop misuse, and the documentation says so.

**The realtime server decides turn ends by polling.** Several times a second
it runs voice activity detection over the audio not yet committed, with the
installed VAD model when exactly one exists and a loudness-based detector
otherwise. A span followed by `silence_duration_ms` of quiet becomes an
utterance and is transcribed. Polling keeps runners stateless and leases
short; the alternative, a streaming detector held open per session, would
have tied up a model for the life of a connection.

**The daemon still has no dependencies.** The WebSocket server
(`internal/ws`, about 250 lines) is written against the standard library.

**Models can come from archives.** The only distribution of the cloning
model is a `tar.bz2` on a release page, so manifests gained a `url` source:
a fixed HTTPS address pinned by SHA-256, optionally an archive unpacked into
the model directory. Extraction creates only regular files and directories
inside that directory; links and paths that escape it are refused
(`TestPullArchiveTraversal`). The unpacked files' hashes are recorded so
`oaudio verify` still works.

## What building it showed

- **A model that loads is not a model that works.** ZipVoice, the first
  cloning model tried, loaded and produced audio of the right length that
  said nothing intelligible: its export needs pronunciation data that the
  engine no longer ships, and nothing warned about it. Only transcribing the
  output revealed it. It was replaced by Pocket TTS, which reads text
  directly. A round trip through a recognizer is now part of trying any
  synthesis model.
- **Engines drop words silently.** sherpa-onnx 2.x pronounces Kokoro text by
  dictionary lookup and skips anything not in the dictionary, so a name like
  "Oaudio" was simply not spoken. The runner now respells unknown words from
  known ones ("o voice"), or as letters. This is a workaround, not
  pronunciation; see "Limits".
- **Recognizers stretch the last word before a pause.** Assigning words to
  speakers by the midpoint of their time range put the final word of a turn
  into the next speaker's mouth. Using the word's start fixed it.
- **Licences inside one download can disagree.** The Pocket TTS archive
  carries a CC-BY-4.0 licence file and a README saying "It is for
  non-commercial." The manifest records the conflict and takes the stricter
  reading.

## Limits

- Diarization was measured on conversations without overlapping speech.
  Overlap, noise and far-field audio are untested.
- The only cloning model is English-only and its licence status is unclear.
  There is no Korean synthesis model in the catalog at all.
- Realtime sessions transcribe; they do not speak or converse. Turn ends
  arrive `silence_duration_ms` plus up to one polling interval (200 ms) after
  the speaker stops. A session's audio is 24 kHz PCM only.
- The unknown-word workaround for Kokoro changes how names sound and spells
  out what it cannot split.
- Nothing prevents someone from typing a false consent statement.

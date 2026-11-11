# API

The server listens on `http://127.0.0.1:11535` (`OAUDIO_HOST` changes it).
There is no authentication; any `Authorization` header is ignored.

## OpenAI-compatible

Point an OpenAI client at `http://127.0.0.1:11535/v1` with any API key.
Field-by-field behaviour and the differences from OpenAI are in
`adr/0004-api.md`.

### `POST /v1/audio/speech`

```sh
curl http://127.0.0.1:11535/v1/audio/speech \
  -H "Content-Type: application/json" \
  -d '{"model":"apple-say","voice":"Samantha","input":"Hello from Oaudio."}' \
  --output hello.mp3
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:11535/v1", api_key="oaudio")

with client.audio.speech.with_streaming_response.create(
    model="apple-say", voice="Yuna", input="안녕하세요.", response_format="wav",
) as response:
    response.stream_to_file("hello.wav")
```

| Field | Values |
|---|---|
| `model` | required |
| `input` | required |
| `voice` | a voice id; omit for the model's default |
| `response_format` | `mp3` (default), `opus`, `aac`, `flac`, `wav`, `pcm` |
| `speed` | 0.25 to 4.0 |
| `stream_format` | `audio` (default), `sse` |
| `language` | extension: choose a voice by language when no voice is given |
| `keep_alive` | extension: how long the model stays loaded after this request |

Formats other than `wav` and `pcm` need `ffmpeg` on the server's `PATH`.

### `POST /v1/audio/transcriptions`

```sh
curl http://127.0.0.1:11535/v1/audio/transcriptions \
  -F model=apple-speech -F language=en -F response_format=srt -F file=@meeting.m4a
```

```python
with open("meeting.wav", "rb") as f:
    result = client.audio.transcriptions.create(
        model="apple-speech", file=f, language="en",
        response_format="verbose_json", timestamp_granularities=["word", "segment"],
    )
print(result.text, result.words[0])
```

| Field | Values |
|---|---|
| `file` | required; WAV, or any format ffmpeg reads |
| `model` | required |
| `language` | ISO 639-1; required by models that cannot detect it |
| `response_format` | `json` (default), `text`, `srt`, `vtt`, `verbose_json` |
| `timestamp_granularities[]` | `word`, `segment`; with `verbose_json` |
| `stream` | `true` for server-sent events; with `json` |
| `prompt`, `temperature` | if the model supports them |
| `keep_alive` | extension |

### `POST /v1/audio/translations`

Same fields without `language`. Output is English. Only for models that
declare `translate`.

### Who said what: `response_format=diarized_json`

```sh
curl http://127.0.0.1:11535/v1/audio/transcriptions \
  -F model=whisper -F response_format=diarized_json -F file=@call.wav
```

```json
{"task":"transcribe","duration":17.7,"text":"Good morning. … slides tonight.",
 "segments":[
  {"type":"transcript.text.segment","id":"seg_0","start":0.1,"end":4.0,"speaker":"A","text":"Good morning, did you get a chance to review the quarterly report?"},
  {"type":"transcript.text.segment","id":"seg_1","start":4.1,"end":10.0,"speaker":"B","text":"I did. The numbers look better than we expected, especially in Europe."}]}
```

| Field | Values |
|---|---|
| `diarization_model` | extension: the diarization model; may be left out when exactly one is installed |
| `num_speakers` | extension: the number of speakers, if known |
| `speaker_threshold` | extension: how different two voices must be to count as two speakers, when the number is not given |

Speakers are labelled `A`, `B`, … in order of first appearance.
`known_speaker_names` is not supported.

### `POST /v1/audio/voices`

Creates a custom voice from a recording, for models that can clone voices.

```sh
curl http://127.0.0.1:11535/v1/audio/voices \
  -F name=anna -F audio_sample=@anna.wav \
  -F consent="Anna Kim agreed on 2026-11-11" \
  -F transcript="What Anna says in the recording."
```

```python
voice = client.audio.voices.create(name="anna", audio_sample=open("anna.wav", "rb"),
                                   consent="Anna Kim agreed on 2026-11-11")
client.audio.speech.create(model="pocket-tts", voice={"id": voice.id}, input="Hello.")
```

| Field | Values |
|---|---|
| `name` | required; unique. On a speech request a name means this voice only for a model that can clone; for any other model it means the model's own voice of that name, so a custom voice cannot hide one. `{"id": …}` always means the custom voice |
| `audio_sample` | required; 1 to 30 seconds of one person speaking, in any format the server can read |
| `consent` | required; a statement of who agreed to have their voice copied. It is stored and shown, not verified |
| `transcript` | extension: what is said in the recording; some models need it |
| `transcription_model` | extension: a recognition model to work the transcript out, if you do not give one |
| `language` | extension |

The response is OpenAI's voice object (`id`, `object: "audio.voice"`, `name`,
`created_at`, `type`) with the extension fields added. A speech request may
then give the voice as `{"id": "voice_…"}`, as the bare id, or by name.

`GET /v1/audio/voices` lists voices and `DELETE /v1/audio/voices/{id-or-name}`
removes one; both are extensions.

Clone a voice only with the consent of the person it belongs to.

### `POST /v1/audio/vad` (extension)

Finds the stretches of a recording that contain speech.

```sh
curl http://127.0.0.1:11535/v1/audio/vad -F file=@call.wav
```

```json
{"duration":17.7,"speech_duration":15.3,
 "segments":[{"start":0.13,"end":3.52},{"start":4.20,"end":9.31}]}
```

Fields: `file` (required), `model` (may be left out when exactly one vad
model is installed), `threshold` (0 to 1), `min_silence_ms`, `min_speech_ms`.

### `POST /v1/audio/diarizations` (extension)

Finds who spoke when, without transcribing.

```json
{"duration":17.7,"speakers":["A","B"],
 "segments":[{"start":0.0,"end":3.5,"speaker":"A"},{"start":4.2,"end":9.3,"speaker":"B"}]}
```

Fields: `file` (required), `model`, `num_speakers`, `speaker_threshold`.

### `GET /v1/realtime` (WebSocket)

Live transcription: OpenAI's Realtime API, transcription sessions only.

```python
import base64
with client.realtime.connect(model="whisper") as conn:
    conn.session.update(session={"type": "transcription", "audio": {"input": {
        "transcription": {"model": "whisper", "language": "en"},
        "turn_detection": {"type": "server_vad", "silence_duration_ms": 500},
    }}})
    conn.input_audio_buffer.append(audio=base64.b64encode(pcm_chunk).decode())   # repeatedly
    for event in conn:
        if event.type == "conversation.item.input_audio_transcription.completed":
            print(event.transcript)
```

Audio in is 16-bit mono PCM at 24 kHz, base64-encoded, in pieces of any size.

| Client event | Effect |
|---|---|
| `session.update` | Set `audio.input.transcription` (`model`, `language`, `prompt`) and `audio.input.turn_detection` (`server_vad` with `threshold`, `prefix_padding_ms`, `silence_duration_ms`, or `null` to turn it off). `session.type` must be `transcription`. |
| `input_audio_buffer.append` | Add audio. |
| `input_audio_buffer.commit` | End the utterance now. Needed when turn detection is off. |
| `input_audio_buffer.clear` | Discard audio not yet committed. |

| Server event | When |
|---|---|
| `session.created`, `session.updated` | On connect, and after each `session.update`. |
| `input_audio_buffer.speech_started` / `.speech_stopped` | Turn detection heard an utterance begin / end. |
| `input_audio_buffer.committed`, `conversation.item.added` | An utterance was handed to the recognizer. |
| `conversation.item.input_audio_transcription.delta` | A piece of the transcript, appended to what came before. With a streaming model (below), while the person is still speaking. |
| `conversation.item.input_audio_transcription.completed` | The whole transcript of one utterance. |
| `conversation.item.input_audio_transcription.failed`, `error` | Something went wrong; the session continues. |

Turn detection uses the installed voice activity detection model when there
is exactly one, and loudness otherwise. An utterance is cut at 30 seconds.

**Words while someone is speaking.** With a model that recognizes as it
listens (`stream` in `oaudio show`, such as `nemotron-asr`) and server turn
detection on, the audio of an utterance goes to the model as it arrives, and
the transcript's growth is sent as deltas straight away, before
`speech_stopped`. An item's events then run: `speech_started`, deltas,
`speech_stopped`, `committed`, `conversation.item.added`, any last deltas,
`.completed`. Deltas only ever append, and `.completed` carries the final
transcript whole. Other models, and sessions with turn detection off,
transcribe each utterance after it ends, as above.

```python
for event in conn:
    if event.type == "conversation.item.input_audio_transcription.delta":
        print(event.delta, end="", flush=True)        # words as they are said
    elif event.type == "conversation.item.input_audio_transcription.completed":
        print()
```
Conversation sessions, `response.create` and audio output are not
implemented: they need a language model.

### `GET /v1/models`, `GET /v1/models/{id}`

Models that can be used right now.

## Native

| Method and path | Purpose |
|---|---|
| `GET /api/version` | `{"version":"…"}` |
| `GET /api/models` | models on this machine, with size, license and languages |
| `GET /api/catalog` | every model Oaudio has a manifest for, with install and engine status |
| `GET /api/show?model=NAME` | the full manifest, install path, and any reason the engine cannot start |
| `GET /api/voices?model=NAME` | voices of a synthesis model |
| `POST /api/pull` `{"model":"NAME"}` | download and verify; progress as NDJSON |
| `GET /api/ps` | models in memory and when each will be unloaded |
| `POST /api/stop` `{"model":"NAME"}` | unload now |
| `DELETE /api/models?model=NAME` | delete a downloaded model |

`POST /api/pull` streams lines such as

```json
{"status":"downloading","file":"ggml-base.en-q5_1.bin","completed":31457280,"total":59721011}
{"status":"verified","completed":59721011,"total":59721011}
{"status":"success","completed":59721011,"total":59721011}
```

and ends with `{"error":"…"}` instead of `success` if anything failed,
including a hash mismatch.

## Errors

```json
{"error":{"message":"model \"kokoro:v1.0\" is not installed; run `oaudio pull kokoro:v1.0`","type":"invalid_request_error","param":null,"code":null}}
```

400 for a request that cannot be served as asked, 404 for an unknown or
missing model, 409 for stopping a model that is in use, 500 when an engine
fails.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `OAUDIO_HOST` | `127.0.0.1:11535` | listen address, and where the CLI connects |
| `OAUDIO_HOME` | `~/.oaudio` | models, user manifests, runners, logs |
| `OAUDIO_KEEP_ALIVE` | `5m` | idle time before a model is unloaded; `-1` never, `0` at once |
| `OAUDIO_MAX_LOADED` | `3` | models in memory at once; the least recently used idle one makes room |
| `OAUDIO_ALIASES` | none | `tts-1=apple-say,whisper-1=apple-speech` |
| `OAUDIO_RUNNERS` | none | extra directories to search for engine runners |
| `OAUDIO_FFMPEG` | found on `PATH` | path to ffmpeg, or `off` |
| `HF_ENDPOINT`, `HF_TOKEN` | huggingface.co, none | mirror, and token for gated models |

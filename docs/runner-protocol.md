# Runner protocol, version 1

A runner is a program that holds one speech model in memory and converts
between text and audio, or describes audio (where the speech is, who is
speaking). The Oaudio daemon starts it, talks to it, and stops
it. Write one to add an engine; it can be in any language.

The reasons for this design are in `adr/0003-runner-contract.md`. Go engines
do not need to implement the wire format: implement `runner.Synthesizer` or
`runner.Transcriber` and call `runner.Main` (see `internal/engines/ref/ref.go`,
about 150 lines, for a complete example).

## Lifecycle

1. The daemon runs the runner with two arguments:

   ```
   oaudio-runner-<engine> --manifest <file> --model-dir <dir>
   ```

   `--manifest` is the model's manifest as JSON. `--model-dir` holds the
   model's files under their upstream names. System models have no files.

2. The runner loads the model. This may take as long as it needs, up to five
   minutes.

3. When it is ready to serve, the runner listens on a loopback TCP port of its
   choosing and prints exactly one line to **stdout**:

   ```json
   {"oaudio_runner":1,"addr":"127.0.0.1:53211"}
   ```

   Nothing else may be written to stdout. Logs go to **stderr**, which the
   daemon copies into its own log.

4. The runner serves HTTP on that address until its **stdin reaches end of
   file** or it receives SIGTERM, then exits. The daemon holds the other end of
   stdin, so a runner never outlives it. If the runner has not exited three
   seconds after stdin closes, it is killed.

If loading fails, the runner writes the reason to stderr and exits non-zero
without printing the handshake.

A runner handles one request at a time. The daemon does not send a second
request until the first has finished.

## Endpoints

### `GET /v1/info`

```json
{"protocol":1,"engine":"whisper.cpp","engine_version":"v1.9.5","tasks":["stt"]}
```

| Field | Meaning |
|---|---|
| `protocol` | Must be `1`. |
| `engine`, `engine_version` | Shown to users and written to logs. |
| `tasks` | Any of `"stt"`, `"tts"`, `"vad"`, `"diarize"`. |
| `sample_rate` | Synthesis only: the rate of the PCM this runner produces. |
| `whole_input` | Synthesis only, optional. `true` asks the daemon to send the whole input in one call instead of one sentence at a time. Set it when each call has a high fixed cost. |

### `GET /v1/voices` (synthesis)

```json
[{"id":"af_heart","name":"Heart","language":"en-US","gender":"female"}]
```

Only `id` is required.

### `POST /v1/synthesize` (synthesis)

Request body:

```json
{"text":"Hello there.","voice":"af_heart","language":"en","speed":1.0}
```

`voice`, `language` and `speed` may be absent; use the model's defaults.

Response: status 200, header `X-Sample-Rate: 24000`, and a body of raw PCM,
**mono, signed 16-bit, little-endian**, written as it is produced. Flush early:
whatever the runner has written is forwarded to the client immediately.

The daemon normally sends one sentence per request. If synthesis fails after
the first byte has been sent, close the connection without finishing the
response, so the daemon sees a truncated body rather than a short but
well-formed one.

### `POST /v1/transcribe` (recognition)

Query parameters:

| Parameter | Meaning |
|---|---|
| `sample_rate` | Rate of the request body. Always the rate in the model's manifest. |
| `language` | ISO 639-1 code, or absent to detect. |
| `task` | `transcribe` (default) or `translate` (to English). |
| `prompt` | Text to bias recognition. |
| `temperature` | 0 to 1. |
| `word_timestamps` | `1` to include per-word times. |

Request body: raw PCM, mono, signed 16-bit, little-endian, the whole
recording.

Response: status 200, `Content-Type: application/x-ndjson`, one JSON object
per line, written as recognition proceeds:

```json
{"type":"segment","segment":{"start":0.0,"end":2.46,"text":" The quick brown fox.","words":[{"word":"The","start":0.0,"end":0.2}]}}
{"type":"segment","segment":{"start":2.52,"end":4.58,"text":" It jumps."}}
{"type":"done","language":"en"}
```

Zero or more `segment` lines, then exactly one `done` or
`{"type":"error","message":"…"}`. Times are seconds from the start of the
audio. Segment text is passed through as the engine produced it; the daemon
joins segments, including inserting spaces where the script uses them.

### `POST /v1/vad` (speech detection)

Query parameters: `sample_rate` (always the manifest's), and optionally
`threshold` (speech probability, 0 to 1), `min_silence_ms` (silence that ends
a span) and `min_speech_ms` (shorter spans are dropped). Absent means the
model's default.

Request body: raw PCM as for `/v1/transcribe`.

Response:

```json
{"spans":[{"start":0.13,"end":3.52},{"start":4.20,"end":9.31}]}
```

Spans are in seconds, in order, and do not overlap. The request is
stateless: the daemon's realtime endpoint calls it repeatedly on a growing
buffer, so it should be fast.

### `POST /v1/diarize` (who spoke when)

Query parameters: `sample_rate`, and optionally `num_speakers` when the
caller knows it, or `threshold` (clustering distance) when it does not.

Request body: raw PCM, the whole recording.

Response:

```json
{"turns":[{"start":0.0,"end":3.5,"speaker":0},{"start":4.2,"end":9.3,"speaker":1}]}
```

Speakers are whole numbers from 0, **in order of first appearance**. The
daemon turns them into the labels `A`, `B`, … and combines them with a
transcript.

### Cloned voices

A synthesis request may carry a `reference` instead of a `voice`:

```json
{"text":"Hello there.","reference":{"audio_path":"/…/voices/voice_ab12/reference.wav","text":"What the recording says."}}
```

`audio_path` is a WAV file, mono, 16-bit, 24 kHz, on the machine the daemon
and runner share. `text` is the recording's transcript and may be empty; a
model that needs it answers 400 when it is. The daemon only sends a
reference to models whose manifest declares `voice_clone`. A runner that
cannot clone answers 400 if it receives one.

## Errors

Before the response has started, answer with a non-200 status and a body of
`{"error":"message"}`. Use **400** when the request is at fault (an unknown
voice, a language the model does not have) and **500** when the engine failed.
The daemon forwards the status and message to the client, where an OpenAI SDK
raises the matching exception.

## What a runner does not do

All of this is done once, by the daemon, for every engine: decoding uploaded
audio (WAV natively, other formats through ffmpeg), mixing to mono,
resampling to the model's rate, encoding responses (`mp3`, `opus`, `aac`,
`flac`, `wav`, `pcm`), splitting text into sentences, building `srt`, `vtt`
and `verbose_json`, checking capabilities, deciding which models are in
memory, and the public HTTP API.

## Trying a runner by hand

In one terminal (it runs until you press Ctrl-D, which closes its stdin):

```sh
oaudio runner reference        # prints {"oaudio_runner":1,"addr":"127.0.0.1:PORT"}
```

In another:

```sh
curl -s 127.0.0.1:PORT/v1/info
curl -s 127.0.0.1:PORT/v1/synthesize -d '{"text":"one two three"}' -o out.pcm
curl -s "127.0.0.1:PORT/v1/transcribe?sample_rate=24000" --data-binary @out.pcm
```

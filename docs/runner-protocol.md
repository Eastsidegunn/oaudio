# Runner protocol, version 1

A **runner** is a program that holds one speech model in memory and does one
of four things with audio: recognizes speech in it, speaks text as it, finds
where the speech is, or finds who is speaking when. The Oaudio daemon starts a
runner when a model is first needed, sends it requests over HTTP on the
loopback interface, and stops it to unload the model.

This page is the whole contract. A runner can be written in any language
without reading Oaudio's code; [`examples/runner.py`](examples/runner.py) is a
complete recognizer in about 100 lines of standard-library Python. Go engines
can skip the wire format: implement `runner.Transcriber` (or `Synthesizer`,
`Detector`, `Diarizer`) and call `runner.Main`, as `internal/engines/ref/ref.go`
does. The reasons for this design are in [`adr/0003-runner-contract.md`](adr/0003-runner-contract.md).

**MUST**, **SHOULD** and **MAY** are used as in RFC 2119. Where this page says
what the daemon does, it describes Oaudio as released; a runner should not
depend on more than this page promises.

[`tools/runner-check`](#checking-a-runner) runs a runner the way the daemon
does and checks its answers against this page.

## Adding an engine

1. Name the engine, for example `myengine`, and build an executable named
   `oaudio-runner-myengine`.
2. Put it where the daemon looks, the first match winning:
   - a directory in `OAUDIO_RUNNERS` (a path list, like `PATH`);
   - the directory of the `oaudio` executable, symbolic links resolved;
   - `../libexec/oaudio` relative to that directory;
   - `$OAUDIO_HOME/runners` (`~/.oaudio/runners` by default);
   - `PATH`.
3. Write a [manifest](adr/0002-manifest-and-store.md) for a model with
   `"engine": "myengine"` and drop it in `$OAUDIO_HOME/manifests/`. A manifest
   there replaces a built-in one of the same name and tag.
4. `oaudio pull <name>:<tag>` downloads and verifies the files, and
   `oaudio run <name>:<tag> …` starts the runner.

The engines built into the `oaudio` binary (`apple`, `reference`) are started
as `oaudio runner <engine>` and follow the same protocol.

## Lifecycle

### Start

The daemon runs:

```
oaudio-runner-<engine> --manifest <file> --model-dir <dir>
```

- `--manifest` is a file holding the model's whole manifest as JSON, the
  schema `oaudio.manifest.v1`. The members a runner usually needs:

  | Member | Meaning |
  |---|---|
  | `name`, `tag` | the model, for logs |
  | `task` | `stt`, `tts`, `vad` or `diarize`: what this process will be asked to do |
  | `family` | free text the manifest's author chose, to tell model families of one engine apart |
  | `source.files[].path` | the model's files, relative to `--model-dir`, under their upstream names |
  | `audio.sample_rate` | for `stt`, `vad` and `diarize`, the rate of all audio the runner will receive |
  | `languages` | ISO 639-1 codes the model knows; a request for another may be refused with 400 |
  | `voices`, `default_voice` | for `tts`, the built-in voices the manifest declares |
  | `capabilities` | what the daemon will ask for; for example a reference recording is only sent with `voice_clone` |
  | `engine_config` | string to string, the engine's own settings; their meaning is up to the runner |

  Unknown members MUST be ignored: the manifest gains members over time.
- `--model-dir` is the directory holding the model's files. For a model with
  no files (`"source": {"type": "system"}`) it may not exist.
- The runner inherits the daemon's environment. Its working directory is not
  specified; use the paths it is given.
- Version 1 passes exactly these two arguments. (The Go helper refuses
  arguments it does not know, so new ones will come with a new version.)

### Handshake

The runner loads the model, which may take up to **five minutes**. Then it
listens on a TCP port of its choosing on `127.0.0.1` and writes exactly one
line to **stdout**:

```json
{"oaudio_runner":1,"addr":"127.0.0.1:53211"}
```

- The line MUST come only after the model is loaded and the port accepts
  connections: the daemon sends requests as soon as it reads it.
- The runner MUST NOT write anything else to stdout, before or after. Logs go
  to **stderr**, which the daemon copies into its log line by line. (The
  daemon reads only the first line and discards the rest, so a later stray
  line does not break anything, but it is lost.)
- If loading fails, the runner writes the reason to stderr and exits with a
  non-zero status, without a handshake. The daemon reports "runner exited
  before it was ready" and points to its log.
- A runner that has not written its handshake within five minutes is killed.

The daemon then requests `GET /v1/info` and stops the runner if `protocol` is
not `1`.

### Requests

The daemon sends **one request at a time** to a runner, and the next only
after the previous response has been read to its end. A runner may rely on
this, and need not be reentrant.

### Stop

The daemon holds the other end of the runner's **stdin**. To unload the model
it closes stdin; the runner MUST then exit. If the runner has not exited
**three seconds** later, it is killed. Because the kernel closes stdin when
the daemon dies, however it dies, a runner that watches stdin never outlives
the daemon.

- A runner that starts processes of its own MUST make sure they end with it,
  also when it is killed. (The audio.cpp runner starts its server under a
  shell that stops it when an inherited pipe closes; the speech.cpp runner
  starts a worker that exits when its own stdin closes.)
- A runner MAY also exit on SIGTERM or SIGINT. The daemon does not send them.
- Memory is returned to the system by the process ending; nothing else is
  required.

## Data

- **Audio** in both directions is raw PCM: mono, signed 16-bit,
  little-endian, no header. A runner that works in floating point reads a
  sample `s` as `s / 32768` and writes `x` as `round(x × 32768)` clamped to
  the 16-bit range, so that a round trip changes nothing.
- **Times** are seconds from the start of the audio, as JSON numbers.
- **Text** is UTF-8.
- **Languages** are ISO 639-1 codes (`en`, `ko`, `ja`).

## Endpoints

| Endpoint | Task |
|---|---|
| `GET /v1/info` | all |
| `GET /v1/voices`, `POST /v1/synthesize` | `tts` |
| `POST /v1/transcribe` | `stt` |
| `POST /v1/stream`, `DELETE /v1/stream` | `stt`, for models that declare `stream` |
| `POST /v1/vad` | `vad` |
| `POST /v1/diarize` | `diarize` |

A runner answers the endpoints of its task. For the others it SHOULD answer
**501** with an error body (any non-200 status is handled).

### `GET /v1/info`

```json
{"protocol":1,"engine":"whisper.cpp","engine_version":"v1.9.5","tasks":["stt"]}
```

| Member | |
|---|---|
| `protocol` | MUST be `1`. |
| `engine`, `engine_version` | Shown in the daemon's log. |
| `tasks` | Any of `stt`, `tts`, `vad`, `diarize`. The daemon does not check it today; give the tasks the runner serves. |
| `sample_rate` | `tts`: the rate of the PCM the runner produces. |
| `whole_input` | `tts`, optional: `true` asks the daemon to send the whole input in one request instead of one sentence at a time. Set it when each request has a high fixed cost. |

### `GET /v1/voices`

```json
[{"id":"af_heart","name":"Heart","language":"en-US","gender":"female"}]
```

An array, possibly empty; only `id` is required.

### `POST /v1/synthesize`

Request: `Content-Type: application/json`.

```json
{"text":"Hello there.","voice":"af_heart","language":"en","speed":1.0}
```

| Member | |
|---|---|
| `text` | Always present and not empty. The daemon normally sends one sentence per request (see `whole_input`). |
| `voice` | A voice id, or absent for the model's default. |
| `language` | Absent for the model's default. |
| `speed` | 1 is normal; absent or 0 means the model's default. A value other than 1 is sent only to a model whose manifest declares `speed`. |
| `reference` | A recording to imitate; see [Cloned voices](#cloned-voices). |

Response: status **200**, header `X-Sample-Rate` with the rate (it MUST be
present and positive, and SHOULD equal `/v1/info`'s `sample_rate`), and a body
of PCM written **as it is produced**: whatever the runner flushes is
forwarded to the client at once, so the first audio should be written as soon
as it exists. `Content-Type: audio/pcm` is customary and not checked. An empty
body is a valid answer for text with nothing to say.

If synthesis fails **after** the first byte, the runner MUST end the
response abnormally, by closing the connection without finishing the body,
so that the daemon sees a broken stream rather than a short one that looks
complete.

### `POST /v1/transcribe`

Query parameters:

| Parameter | |
|---|---|
| `sample_rate` | The rate of the body. Always the manifest's `audio.sample_rate`; required, and a request without it is answered 400. |
| `language` | ISO 639-1, or absent to detect. |
| `task` | `transcribe` (the default) or `translate` (into English). |
| `prompt` | Text to bias recognition. |
| `temperature` | 0 to 1. |
| `word_timestamps` | `1` to include per-word times. |

The daemon sends `task=translate`, `prompt` and `word_timestamps` only to
models whose manifest declares `translate`, `prompt` and `word_timestamps`.

Request body: the whole recording as PCM.

Response: status **200**, `Content-Type: application/x-ndjson`, one JSON object
per line, written as recognition proceeds:

```json
{"type":"segment","segment":{"start":0.0,"end":2.46,"text":" The quick brown fox.","words":[{"word":"The","start":0.0,"end":0.2}]}}
{"type":"segment","segment":{"start":2.52,"end":4.58,"text":" It jumps."}}
{"type":"done","language":"en"}
```

- Zero or more `segment` lines, in order of time, then exactly one `done`
  (with the language heard, or `""` when unknown) or one
  `{"type":"error","message":"…"}`. A stream that ends without either is a
  failure.
- Segment text is passed on as the engine wrote it, leading spaces included;
  the daemon joins segments, putting spaces between them where the script
  uses spaces.
- `words` is optional, and expected only when `word_timestamps=1`.
- A runner that refuses the request before recognizing anything (an
  unsupported language, `translate` on a model that cannot) SHOULD answer with
  a status and an error body instead, as under [Errors](#errors), so the
  client gets 400 and not a server error. An `error` line is always reported
  to the client as a server error (500).

### `POST /v1/stream`

Recognition of one utterance while it is still being spoken, for a model
whose manifest declares the `stream` capability. The daemon uses it in
realtime sessions: from the moment its speech detector hears someone start,
it sends the new audio about every 200 ms and passes each growth of the
transcript to the client at once. A runner that does not declare `stream`
never receives it, and SHOULD answer 501 if it does. A runner that streams
still answers `/v1/transcribe` for whole recordings.

Query parameters:

| Parameter | |
|---|---|
| `session` | An id the daemon chose for this utterance. Required. |
| `sample_rate` | The manifest's `audio.sample_rate`. Required. |
| `start` | `1` on the first piece: begin a new stream for `session` (replacing one of that id, if any). |
| `language` | ISO 639-1, or absent to detect. Read with `start`. |
| `end` | `1` on the last piece: the utterance is over. |

Request body: the next piece of the utterance as PCM, possibly empty
(always on its own with `end=1` when the end came with no new audio).

Response: status **200**, `Content-Type: application/json`:

```json
{"text":"조국이 당신을 위해","done":false}
{"text":"조국이 당신을 위해 무엇을 해줄 수 있는지 묻지 말고","done":true,"language":"ko"}
```

- `text` is the **whole transcript so far**, not the change. The daemon
  keeps what it has already sent and works out what is new.
- The transcript SHOULD only grow: each `text` begins with the previous one.
  The daemon sends the client only additions (the API's deltas are
  append-only), so a revision is held back until the text grows past it, and
  the final transcript is always sent whole.
- With `end=1` the runner finishes the utterance (flushing whatever the
  model holds back), answers with `"done": true`, the final transcript and
  the language heard (`""` or absent when unknown), and forgets the stream.
- A piece without `start=1` for a session the runner does not know MUST be
  answered **404** with an error body. This happens after the runner
  restarted, or dropped a stream it kept too long; the audio before is lost,
  and transcribing the rest alone would give a wrong transcript that looks
  right. The daemon reports the item's transcription as failed.
- A refusal before the stream exists (a language the model lacks, a missing
  parameter) is 400, as under [Errors](#errors).
- Requests remain one at a time: pieces of different streams can follow each
  other, and other requests can come between them. A runner keeps its open
  streams between requests. It MAY drop a stream untouched for a long while
  (the Go helper drops them after two minutes, or the least recently used
  beyond sixteen); the daemon ends every stream it starts.

### `DELETE /v1/stream`

Query parameter `session`. Abandons the stream (the client cleared its
audio, or went away); the answer is **204** whether or not the runner knew
the stream.

### `POST /v1/vad`

Query parameters: `sample_rate` (the manifest's), and optionally `threshold`
(speech probability, 0 to 1), `min_silence_ms` (silence that ends a span) and
`min_speech_ms` (shorter spans are dropped). Absent means the model's default.

Request body: PCM. Response:

```json
{"spans":[{"start":0.13,"end":3.52},{"start":4.20,"end":9.31}]}
```

Spans in order, not overlapping; `[]` when there is no speech. The request is
stateless, and the daemon's realtime endpoint calls it repeatedly on a
growing buffer, so it should be fast.

### `POST /v1/diarize`

Query parameters: `sample_rate`, and optionally `num_speakers` when the
caller knows it, or `threshold` (clustering distance) when it does not.

Request body: the whole recording as PCM. Response:

```json
{"turns":[{"start":0.0,"end":3.5,"speaker":0},{"start":4.2,"end":9.3,"speaker":1}]}
```

Turns ordered by start time. Speakers are whole numbers from 0 **in order of
first appearance**: the first turn is speaker 0, and a speaker first heard
after speakers 0 to n−1 is n. The daemon turns them into `A`, `B`, … and
combines them with a transcript.

### Cloned voices

A synthesis request may carry a `reference` instead of a `voice`:

```json
{"text":"Hello there.","reference":{"audio_path":"/…/voices/voice_ab12/reference.wav","text":"What the recording says."}}
```

- `audio_path` is a WAV file the daemon keeps: mono, 16-bit PCM, 24 kHz, on
  the machine the daemon and the runner share. Read it; do not keep the path.
- `text` is its transcript, absent or empty when the user gave none. A model
  that needs one answers 400 without it.
- The daemon sends a reference only to models whose manifest declares
  `voice_clone`. A runner that cannot clone SHOULD answer 400 if it receives
  one anyway.

## Errors

Before a response has started, a runner answers with a non-200 status and the
body `{"error":"message"}`, with a message written for the end user:

| Status | When | The client gets |
|---|---|---|
| 400 | the request is at fault: an unknown voice, a language the model does not have, a missing parameter | 400, the message |
| 501 | an endpoint of a task the runner does not serve | 400, the message |
| 500 | the engine failed | 500, the message |

Other statuses are passed to the client unchanged. A body that is not this
JSON is replaced by "runner returned <status>". Through an OpenAI SDK, 400
raises `BadRequestError` and 500 `InternalServerError`.

## What a runner does not do

The daemon does all of this once, for every engine: decoding uploads (WAV
natively, other formats through ffmpeg), mixing to mono, resampling to the
model's rate, encoding responses (`mp3`, `opus`, `aac`, `flac`, `wav`, `pcm`),
splitting text into sentences, building `srt`, `vtt` and `verbose_json`,
rejecting requests for capabilities a manifest does not declare, keeping
cloned voices, deciding which models stay in memory, and the public API.

## Checking a runner

`tools/runner-check` starts a runner exactly as the daemon does, exercises the
endpoints of the manifest's task (and `/v1/stream` when the manifest
declares `stream`, feeding the audio in 200 ms pieces), checks the shape of every answer against
this page, and checks that the runner exits when its stdin closes:

```sh
go run ./tools/runner-check --manifest model.json --model-dir DIR [--audio speech.wav] -- RUNNER [ARGS...]
go run ./tools/runner-check --manifest docs/examples/runner-manifest.json --model-dir docs/examples -- python3 docs/examples/runner.py
```

`FAIL` is a MUST of this page, `WARN` a SHOULD. It does not judge how well a
model recognizes or speaks; [`evidence.md`](evidence.md) does that, for the
models in the catalog.

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

## Versions

`protocol` changes only for a change a runner must follow: a member removed
or given another meaning, a new required behaviour. Members and endpoints
are added within version 1, which is why a runner ignores what it does not
know. A daemon refuses a runner of another version.

## Changes to this page

**2026-11-12, streaming recognition** ([ADR 0008](adr/0008-streaming-recognition.md)),
added within version 1: the `stream` capability, `POST` and `DELETE
/v1/stream`, and `languages` among the manifest members a runner usually
needs. A runner that does not stream is unaffected.

Rewritten on 2026-11-12 as a standalone specification, checked against the
code. Where the earlier page and the code disagreed, the code was kept, except
for one fix to the code:

- **A recognition refused before it starts now gets its own status.** The
  earlier page said a request at fault is answered with 400 before the
  response starts, but the Go helper (`runner.Handler`) always answered
  `/v1/transcribe` with 200 and then an `error` line, which the daemon reports
  as 500. The helper now sends the status with the first line, so an engine's
  `runner.BadRequest` becomes a 400. Runners in other languages were always
  free to do this.
- **501 for a task a runner lacks**, the Go helper's behaviour, is now
  written down, with the daemon's translation of it into 400.
- **A runner that cannot clone and receives a reference** was said to answer
  400. The daemon never sends one to such a model, and the built-in Apple
  engine ignores it, so this is a SHOULD.
- **`tasks` in `/v1/info`** is not checked by the daemon; the page now says so.
- Added: where the daemon looks for runners and user manifests, what the
  manifest file contains, the five-minute load limit, which optional
  parameters are only sent to models that declare them, how errors reach the
  client, and the PCM conversion rule.

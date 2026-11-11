# Contributing

## Build and test

```sh
make            # bin/oaudio, and on macOS bin/oaudio-apple-stt
make runners    # the whisper.cpp, sherpa-onnx, audio.cpp and speech.cpp runners (needs cmake and a C++ compiler)
make test       # format check, go vet and unit tests
make e2e        # the real binary against the macOS engines (macOS 26+)
```

The daemon and CLI use only the Go standard library and build with
`CGO_ENABLED=0`. Please keep it that way: engines and their dependencies
belong in runners (`docs/engines.md`), which are separate programs.

## Adding a model

1. `oaudio manifest hf <owner/repo> --include '<glob>,<glob>'` prints a
   manifest skeleton with the file list and the hashes the Hub publishes.
2. Fill in the rest: `name`, `tag`, `task`, `engine`, `upstream` (credit the
   people who trained the model, not only whoever converted it), `license`
   with an honest `commercial_use`, `languages`, `capabilities`, `audio`.
3. Try it by saving it in `~/.oaudio/manifests/`, then `oaudio pull` and
   `oaudio run`.
4. To propose it for the built-in catalog, add it under
   `internal/catalog/models/`. `go test ./internal/catalog` checks every
   embedded manifest.

For a model published as an archive rather than a repository, use a `url`
source: each file has an `https` address and a SHA-256, and an archive has
`"extract": "tar.bz2"` (see `internal/catalog/models/pocket-tts-int8.json`).

Before proposing a synthesis model, transcribe what it says and compare with
what you asked for. A model can load, run and return audio that says nothing.
For a cloning model, also check that the clone resembles the speaker
(`bench/clone_eval.py`). For a recognition model, run a public test set
through Oaudio and through the engine directly (`bench/asr_eval.py`).

`docs/model-queue.md` lists the candidates, and the models that were tried
and left out with the reason. Check it before starting, and record what you
find there, including a failure.

The engine a model needs decides what goes in `engine_config`: see the
tables in `docs/engines.md`. Many sherpa-onnx recognition models and
audio.cpp synthesis models need only a manifest.

Only declare a capability you have seen work. The server refuses requests for
undeclared capabilities, so a missing one is an inconvenience and a false one
is a bug.

## Adding an engine

See `docs/runner-protocol.md` and `docs/engines.md`. The smallest complete
examples are `docs/examples/runner.py` (any language) and
`internal/engines/ref/ref.go` (Go). A new runner must pass
`go run ./tools/runner-check` with no `FAIL` before it is merged.

## Claims need evidence

`docs/evidence.md` lists what has been measured and how to reproduce it, and
what has not. If a change makes a claim about accuracy, speed or
compatibility, add the script and the result. If something is untested, say
so in the same place.

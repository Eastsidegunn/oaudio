# ADR 0002: Manifests point at upstream files; nothing is repackaged

Status: accepted, 2026-11-11

## Context

Ollama stores models as hashed blobs under its own registry format. For its
speech model, each tensor is a separate registry layer
(`GET registry.ollama.ai/v2/library/phonon/manifests/latest` returns layers of
type `application/vnd.ollama.image.tensor` named `flow_lm.bos_emb`, …). The
consequences are well documented by its users: other tools cannot read the
store, the library lags the publisher, and the publisher's own packaging
choices are replaced.

Speech has no common container format to standardise on anyway. Whisper ships
as GGML, Kokoro as ONNX plus a voices table and lexicons, and each engine reads
its own layout. A unifying format would have to be invented and every model
converted into it.

Hugging Face already publishes what a registry needs: immutable commits, and a
hash per file (a SHA-256 for large files, a git blob SHA-1 for small ones) from
`GET /api/models/{repo}/tree/{commit}`.

## Decision

A model is described by a **manifest**: a small JSON file (`oaudio.manifest.v1`,
defined in `internal/manifest/manifest.go`) that contains no weights. It names

- the **engine** that runs the model;
- the **upstream** model and authors, shown by `oaudio show`;
- the **source**: a repository, a full 40-character commit (never a branch),
  and each file with its size and published hash;
- the **license**, including whether commercial use is allowed, whether the
  model is gated, and any bundled component under a different license;
- task, languages, sample rate, capabilities and voices.

`oaudio pull` downloads those files with resume, checks every byte against the
manifest hash, and only then marks the model installed. A file that does not
match is deleted, never kept.

On disk a model is a plain directory:

```
~/.oaudio/models/<name>/<tag>/manifest.json
~/.oaudio/models/<name>/<tag>/<files, with upstream names and bytes>
```

so `whisper-cli -m "$(oaudio show whisper --path)/ggml-base.en-q5_1.bin"` works.

Manifests come from the set embedded in the binary and from
`~/.oaudio/manifests/*.json`. User manifests override embedded ones, so a new
model does not wait for a release. `oaudio manifest hf <repo>` writes a
manifest skeleton with the Hub's hashes for review.

Capabilities are enforced: a request that needs something the manifest does
not declare (word timestamps, translation, a prompt, speed) is refused with an
explanation instead of being silently ignored.

## Consequences

- No registry to run, no conversion pipeline, no lock-in. Removing Oaudio
  leaves usable model files behind.
- Files are not deduplicated across tags. Speech models rarely share files, so
  the simplicity is worth more.
- Trust rests on the reviewed manifest. The Hub is not trusted at download
  time: it could serve anything and the hash check would reject it
  (`TestPullRejectsWrongContent`).
- A manifest pins one conversion of a model. When the conversion's publisher
  is not the model's author (Whisper GGML by whisper.cpp, Kokoro ONNX by
  sherpa-onnx), the manifest credits both.
- Curation still exists (which tag is `latest`), but every default is visible
  in a file a user can read and override.

## Verified

- `oaudio pull whisper:base.en-q5_1` against the real Hub: the SHA-256 of the
  file on disk equals the `lfs.oid` the Hub API reports
  (`4baf70dd…fdd2f`). See `docs/evidence.md`.
- Resume, corruption rejection and tamper detection: `internal/pull/pull_test.go`.

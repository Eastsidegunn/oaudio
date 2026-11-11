"""What an unmodified OpenAI Python SDK can do against a local speech server.

A neutral check, for comparing servers: every call is the SDK's own, every
check runs on its own (one failure does not stop the rest), errors are judged
by exception type only, never by wording, and the recognizer is fed a fixed
public recording rather than the server's own synthesis, so a weak voice
cannot fail the recognition checks.

  uv run --with openai --with jiwer python bench/compat_matrix.py \
      --base-url http://127.0.0.1:8080/v1 --tts kokoro --voice af_heart \
      --stt whisper --audio sample.flac --text "what the recording says" \
      [--diarization-model pyannote-diarize] [--clone-model qwen3-tts] --out result.json

A check that a server does not attempt is "fail", with the reason recorded.
"""
import argparse, json, os, subprocess, sys, tempfile, traceback

import jiwer
import openai
from openai import OpenAI, BadRequestError, NotFoundError, UnprocessableEntityError

ap = argparse.ArgumentParser()
ap.add_argument("--base-url", required=True)
ap.add_argument("--name", default="")
ap.add_argument("--tts", required=True)
ap.add_argument("--voice", required=True)
ap.add_argument("--stt", required=True)
ap.add_argument("--audio", required=True, help="a recording of English speech")
ap.add_argument("--text", required=True, help="what the recording says")
ap.add_argument("--translate-model", help="a model that translates, if not --stt")
ap.add_argument("--diarization-model")
ap.add_argument("--clone-model")
ap.add_argument("--out")
args = ap.parse_args()

client = OpenAI(base_url=args.base_url, api_key="local", timeout=600, max_retries=0)
tmp = tempfile.mkdtemp()
rows = []


def wer(a, b):
    norm = lambda s: " ".join("".join(c for c in s.lower() if c.isalnum() or c in " '").split())
    return jiwer.wer(norm(a), norm(b)) if norm(a) else 1.0


def check(name, fn):
    try:
        ok, detail = fn()
    except Exception as e:  # anything the SDK raises is a failed check
        ok, detail = False, f"{type(e).__name__}: {str(e)[:160]}"
    rows.append({"check": name, "ok": bool(ok), "detail": str(detail)[:200]})
    print("PASS" if ok else "FAIL", name, "-", str(detail)[:120])


# The same recording as WAV and MP3, made here so both paths get the same bytes.
wav = os.path.join(tmp, "in.wav")
mp3 = os.path.join(tmp, "in.mp3")
subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", args.audio, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", wav], check=True)
subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", wav, "-c:a", "libmp3lame", "-q:a", "4", mp3], check=True)
TEXT = "The quick brown fox jumps over the lazy dog."

# --- models
# A server may list "kokoro:v1.0" for a model requested as "kokoro", as
# Ollama does; either form counts.
same = lambda got, want: got == want or got.startswith(want + ":")
check("models.list", lambda: (lambda ids: (any(same(i, args.tts) for i in ids) and any(same(i, args.stt) for i in ids), ids[:6]))(
    [m.id for m in client.models.list()]))
check("models.retrieve", lambda: (lambda m: (same(m.id, args.tts), m.id))(client.models.retrieve(args.tts)))


# --- speech
def speech(**kw):
    return client.audio.speech.create(model=args.tts, voice=args.voice, input=TEXT, **kw).read()


def is_mp3(b):
    return b[:3] == b"ID3" or (len(b) > 1 and b[0] == 0xFF and b[1] & 0xE0 == 0xE0)


check("speech, SDK defaults (OpenAI answers mp3)", lambda: (lambda b: (is_mp3(b), f"{len(b)} bytes, starts {b[:4]!r}"))(speech()))
check("speech wav", lambda: (lambda b: (b[:4] == b"RIFF", f"{len(b)} bytes"))(speech(response_format="wav")))
check("speech pcm (24 kHz 16-bit)", lambda: (lambda b: (1.2 < len(b) / 48000 < 8, f"{len(b) / 48000:.2f} s if 24 kHz"))(speech(response_format="pcm")))
for fmt, ok in (("opus", lambda b: b[:4] == b"OggS"), ("aac", lambda b: len(b) > 1 and b[0] == 0xFF and b[1] & 0xF0 == 0xF0),
                ("flac", lambda b: b[:4] == b"fLaC")):
    check(f"speech {fmt}", lambda fmt=fmt, ok=ok: (lambda b: (ok(b), f"{len(b)} bytes, starts {b[:4]!r}"))(speech(response_format=fmt)))
check("speech speed=1.5 is shorter", lambda: (lambda a, b: (len(b) < 0.85 * len(a), f"{len(a)} -> {len(b)} bytes"))(
    speech(response_format="pcm"), speech(response_format="pcm", speed=1.5)))


def streamed_speech():
    path = os.path.join(tmp, "s.wav")
    with client.audio.speech.with_streaming_response.create(model=args.tts, voice=args.voice, input=TEXT, response_format="wav") as r:
        r.stream_to_file(path)
    return open(path, "rb").read(4) == b"RIFF", "streamed to file"


check("speech, streamed response", streamed_speech)


# --- transcription
def transcribe(path=wav, **kw):
    with open(path, "rb") as f:
        return client.audio.transcriptions.create(model=args.stt, file=f, **kw)


check("transcription json, wav upload", lambda: (lambda t: (wer(args.text, t.text) <= 0.2, f"WER {wer(args.text, t.text):.2f}: {t.text[:60]}"))(transcribe()))
check("transcription json, mp3 upload", lambda: (lambda t: (wer(args.text, t.text) <= 0.2, f"WER {wer(args.text, t.text):.2f}"))(transcribe(mp3)))
check("transcription text", lambda: (lambda t: (isinstance(t, str) and wer(args.text, t) <= 0.2, repr(t)[:60]))(transcribe(response_format="text")))
check("transcription srt", lambda: (lambda t: ("-->" in t and t.lstrip().startswith("1"), repr(t)[:60]))(transcribe(response_format="srt")))
check("transcription vtt", lambda: (lambda t: (t.startswith("WEBVTT"), repr(t)[:60]))(transcribe(response_format="vtt")))


def verbose():
    v = transcribe(response_format="verbose_json", timestamp_granularities=["segment"])
    return bool(v.segments) and v.segments[0].end > v.segments[0].start and v.duration > 0, f"{len(v.segments or [])} segments"


def words():
    v = transcribe(response_format="verbose_json", timestamp_granularities=["word"])
    ws = v.words or []
    return len(ws) >= 5 and all(w.end >= w.start for w in ws), f"{len(ws)} words"


check("transcription verbose_json segments", verbose)
check("transcription word timestamps", words)


def streamed_transcription():
    kinds, final = [], ""
    for e in transcribe(stream=True):
        kinds.append(e.type)
        if e.type == "transcript.text.done":
            final = e.text
    return "transcript.text.delta" in kinds and kinds[-1] == "transcript.text.done" and wer(args.text, final) <= 0.2, f"{len(kinds)} events"


check("transcription stream=True", streamed_transcription)
check("translation endpoint answers", lambda: (lambda t: (bool(t.text), t.text[:50]))(
    client.audio.translations.create(model=args.translate_model or args.stt, file=open(wav, "rb"))))

# --- optional features, attempted only when a model is named
if args.diarization_model:
    def diarized():
        d = transcribe(response_format="diarized_json", extra_body={"diarization_model": args.diarization_model})
        return type(d).__name__ == "TranscriptionDiarized" and bool(d.segments), f"{len(d.segments or [])} segments"
    check("transcription diarized_json", diarized)
else:
    rows.append({"check": "transcription diarized_json", "ok": False, "detail": "no diarization model to try"})

if args.clone_model:
    def clone():
        with open(wav, "rb") as f:
            v = client.audio.voices.create(name="compat-matrix", audio_sample=f, consent="public recording; test only")
        try:
            b = client.audio.speech.create(model=args.clone_model, voice={"id": v.id}, input=TEXT, response_format="wav").read()
            return b[:4] == b"RIFF" and len(b) > 20000, f"{v.id}, {len(b)} bytes"
        finally:
            try:
                client.delete(f"/audio/voices/{v.id}", cast_to=object)
            except Exception:
                pass
    check("voices.create + speech in that voice", clone)
else:
    rows.append({"check": "voices.create + speech in that voice", "ok": False, "detail": "no cloning model to try"})


# --- errors, by type only
def raises(fn, kinds):
    try:
        fn()
        return False, "no error"
    except kinds as e:
        return True, type(e).__name__
    except openai.APIStatusError as e:
        return False, f"{type(e).__name__} (HTTP {e.status_code})"


check("unknown model -> NotFoundError", lambda: raises(lambda: client.audio.speech.create(model="no-such-model", voice=args.voice, input="x").read(), NotFoundError))
check("unknown voice -> 400/422", lambda: raises(lambda: client.audio.speech.create(model=args.tts, voice="no-such-voice", input="x").read(),
                                                     (BadRequestError, UnprocessableEntityError, NotFoundError)))

passed = sum(r["ok"] for r in rows)
print(f"\n{passed}/{len(rows)} passed  (openai SDK {openai.__version__}, {args.base_url})")
if args.out:
    json.dump({"name": args.name, "base_url": args.base_url, "sdk": openai.__version__, "tts": args.tts, "stt": args.stt,
               "passed": passed, "total": len(rows), "rows": rows}, open(args.out, "w"), indent=1)

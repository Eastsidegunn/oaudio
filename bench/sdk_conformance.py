"""Drive Oaudio with the unmodified official OpenAI Python SDK.

Only base_url changes. Every call below is copied in shape from the SDK's own
documentation. Run:  uv run --with openai python bench/sdk_conformance.py
"""
import os, sys, tempfile, wave
from openai import OpenAI, BadRequestError, NotFoundError
import openai

BASE = os.environ.get("OAUDIO_BASE_URL", "http://127.0.0.1:11535/v1")
TTS = os.environ.get("OAUDIO_TTS_MODEL", "apple-say")
STT = os.environ.get("OAUDIO_STT_MODEL", "apple-speech")
VOICE = os.environ.get("OAUDIO_VOICE", "Samantha")
TEXT = "The quick brown fox jumps over the lazy dog."

client = OpenAI(base_url=BASE, api_key="oaudio")
results = []
def check(name, ok, detail=""):
    results.append(ok)
    print(("PASS" if ok else "FAIL"), name, detail)

print("openai SDK", openai.__version__, "->", BASE)
tmp = tempfile.mkdtemp()

ids = [m.id for m in client.models.list()]
check("models.list", any(i.startswith(TTS) for i in ids) and any(i.startswith(STT) for i in ids), str(ids))
check("models.retrieve", client.models.retrieve(TTS).id.startswith(TTS))

# Speech with the SDK defaults: no response_format, so the server must pick mp3.
resp = client.audio.speech.create(model=TTS, voice=VOICE, input=TEXT)
mp3 = resp.read()
check("speech default format is mp3", mp3[:3] == b"ID3" or (mp3[0] == 0xFF and mp3[1] & 0xE0 == 0xE0), f"{len(mp3)} bytes")

wav_path = os.path.join(tmp, "speech.wav")
with client.audio.speech.with_streaming_response.create(model=TTS, voice=VOICE, input=TEXT, response_format="wav") as r:
    r.stream_to_file(wav_path)
with open(wav_path, "rb") as f:
    head = f.read(44)
check("speech streaming to wav", head[:4] == b"RIFF" and head[8:12] == b"WAVE")

pcm = client.audio.speech.create(model=TTS, voice=VOICE, input=TEXT, response_format="pcm").read()
secs = len(pcm) / 2 / 24000
check("speech pcm is 24 kHz 16-bit", 1.5 < secs < 6, f"{secs:.2f}s if read as 24 kHz")

for fmt in ("opus", "aac", "flac"):
    data = client.audio.speech.create(model=TTS, voice=VOICE, input="Hello.", response_format=fmt).read()
    magic = {"opus": b"OggS", "flac": b"fLaC"}.get(fmt)
    check(f"speech {fmt}", (data[:4] == magic) if magic else (data[0] == 0xFF and data[1] & 0xF0 == 0xF0), f"{len(data)} bytes")

check("speech speed", len(client.audio.speech.create(model=TTS, voice=VOICE, input=TEXT, response_format="pcm", speed=1.5).read()) < len(pcm))

# Transcription in every response format, uploading the mp3 the server made.
mp3_path = os.path.join(tmp, "speech.mp3")
open(mp3_path, "wb").write(mp3)
def norm(s): return "".join(c for c in s.lower() if c.isalnum() or c == " ").split()

with open(mp3_path, "rb") as f:
    t = client.audio.transcriptions.create(model=STT, file=f, language="en")
check("transcription json (mp3 upload)", norm(t.text) == norm(TEXT), repr(t.text))

with open(wav_path, "rb") as f:
    text = client.audio.transcriptions.create(model=STT, file=f, language="en", response_format="text")
check("transcription text", norm(text) == norm(TEXT), repr(text))

with open(wav_path, "rb") as f:
    v = client.audio.transcriptions.create(model=STT, file=f, language="en", response_format="verbose_json",
                                           timestamp_granularities=["word", "segment"])
check("transcription verbose_json", v.language == "english" and v.duration > 1 and len(v.segments) >= 1
      and v.segments[0].end > v.segments[0].start, f"duration={v.duration:.2f} segments={len(v.segments)}")
check("transcription word timestamps", v.words and all(w.end >= w.start for w in v.words) and
      norm(" ".join(w.word for w in v.words)) == norm(TEXT), f"{len(v.words or [])} words")

with open(wav_path, "rb") as f:
    srt = client.audio.transcriptions.create(model=STT, file=f, language="en", response_format="srt")
check("transcription srt", srt.startswith("1\n00:00:00,") and "-->" in srt)
with open(wav_path, "rb") as f:
    vtt = client.audio.transcriptions.create(model=STT, file=f, language="en", response_format="vtt")
check("transcription vtt", vtt.startswith("WEBVTT"))

with open(wav_path, "rb") as f:
    events = list(client.audio.transcriptions.create(model=STT, file=f, language="en", stream=True))
kinds = [e.type for e in events]
check("transcription stream", kinds[-1] == "transcript.text.done" and "transcript.text.delta" in kinds
      and norm(events[-1].text) == norm(TEXT), str(kinds))

# Optional: speaker-labelled transcripts and custom voices, when the models
# for them are named. Both use the SDK's own methods and types.
DIAR = os.environ.get("OAUDIO_DIARIZATION_MODEL")
CLONE = os.environ.get("OAUDIO_CLONE_MODEL")
if DIAR:
    with open(wav_path, "rb") as f:
        d = client.audio.transcriptions.create(model=STT, file=f, language="en", response_format="diarized_json",
                                               extra_body={"diarization_model": DIAR})
    check("transcription diarized_json", type(d).__name__ == "TranscriptionDiarized" and d.segments
          and d.segments[0].speaker == "A" and d.segments[0].type == "transcript.text.segment" and norm(d.text) == norm(TEXT),
          f"{len(d.segments)} segments, speakers {sorted({x.speaker for x in d.segments})}")
if CLONE:
    name = "sdk-conformance"
    try:
        client.delete(f"/audio/voices/{name}", cast_to=object)
    except Exception:
        pass
    with open(wav_path, "rb") as f:
        v = client.audio.voices.create(name=name, audio_sample=f, consent="synthetic voice made by this test")
    check("voices.create", v.object == "audio.voice" and v.id.startswith("voice_") and v.name == name, v.id)
    said = "This is spoken in a custom voice."
    cloned = client.audio.speech.create(model=CLONE, voice={"id": v.id}, input=said, response_format="wav").read()
    path = os.path.join(tmp, "clone.wav")
    open(path, "wb").write(cloned)
    with open(path, "rb") as f:
        heard = client.audio.transcriptions.create(model=STT, file=f, language="en").text
    check("speech with a custom voice", norm(heard) == norm(said), repr(heard))
    try:
        client.audio.speech.create(model=TTS, voice={"id": v.id}, input="x"); check("custom voice on a non-cloning model -> BadRequestError", False)
    except BadRequestError as e:
        check("custom voice on a non-cloning model -> BadRequestError", "cannot clone" in str(e))
    client.delete(f"/audio/voices/{v.id}", cast_to=object)

# Errors must surface as the SDK's own typed exceptions.
try:
    client.audio.speech.create(model="no-such-model", voice=VOICE, input="x"); check("unknown model -> NotFoundError", False)
except NotFoundError as e:
    check("unknown model -> NotFoundError", "not found" in str(e))
try:
    client.audio.speech.create(model=TTS, voice="no-such-voice", input="x"); check("unknown voice -> BadRequestError", False)
except BadRequestError as e:
    check("unknown voice -> BadRequestError", "no-such-voice" in str(e))

print(f"\n{sum(results)}/{len(results)} passed")
sys.exit(0 if all(results) else 1)

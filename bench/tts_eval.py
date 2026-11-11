"""Check a text-to-speech model by round trip and against the engine itself.

For each sentence: synthesize through Oaudio, transcribe the result through
Oaudio, and score the transcript against the input text. A low error rate
means the audio that came out of the API is intelligible speech of the right
words. With --direct apple the same sentence is also synthesized by calling
/usr/bin/say directly, to measure what Oaudio adds in time and to confirm the
audio is the same length.

  uv run --with jiwer --with whisper-normalizer python bench/tts_eval.py \
      --sentences sentences.txt --language ko --voice Yuna \
      --tts apple-say --stt apple-speech --direct apple

With --direct speechcpp the sentence is also spoken by `speech tts` run
directly on the model file (--model-file), with the seed given by --seed.
The model's manifest must give its worker the same seed
(engine_config.options "--seed N"), so that the two can be compared sample
for sample.
"""
import argparse, io, json, os, subprocess, sys, tempfile, time, uuid, urllib.request, wave

import jiwer
from whisper_normalizer.basic import BasicTextNormalizer
from whisper_normalizer.english import EnglishTextNormalizer


def speak(base, model, voice, language, text):
    body = {"model": model, "input": text, "response_format": "wav"}
    if voice:
        body["voice"] = voice
    elif language:
        body["language"] = language
    req = urllib.request.Request(base + "/audio/speech", data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=600) as r:
        # The 44-byte WAVE header can arrive before any audio is made, so
        # the clock stops at the first byte after it.
        first = None
        chunks, got = [], 0
        while True:
            b = r.read1(8192)
            if not b:
                break
            got += len(b)
            if first is None and got > 44:
                first = time.time() - t0
            chunks.append(b)
        rate = int(r.headers["X-Oaudio-Sample-Rate"])
    wav = b"".join(chunks)
    return wav, (len(wav) - 44) / 2 / rate, first, time.time() - t0


def transcribe(base, model, language, wav):
    boundary = uuid.uuid4().hex
    parts = [f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode()
             for k, v in (("model", model), ("language", language))]
    parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="a.wav"\r\n\r\n'.encode() + wav + b"\r\n")
    parts.append(f"--{boundary}--\r\n".encode())
    req = urllib.request.Request(base + "/audio/transcriptions", data=b"".join(parts),
                                 headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
    with urllib.request.urlopen(req, timeout=600) as r:
        return json.load(r)["text"]


def say_direct(voice, text, tmp):
    txt, wav = os.path.join(tmp, "in.txt"), os.path.join(tmp, "out.wav")
    open(txt, "w", encoding="utf-8").write(text)
    t0 = time.time()
    subprocess.run(["/usr/bin/say", "-o", wav, "--file-format=WAVE", "--data-format=LEI16@22050", "-f", txt, "-v", voice], check=True)
    took = time.time() - t0
    return (os.path.getsize(wav) - 4096) / 2 / 22050, took  # say pads its header to 4096 bytes


def pcm_of(wav):
    """The 16-bit samples of a WAVE file's bytes."""
    with wave.open(io.BytesIO(wav)) as w:
        return w.readframes(w.getnframes()), w.getframerate()


def speechcpp_direct(binary, model_file, voice, seed, text, tmp):
    out = os.path.join(tmp, "direct.wav")
    args = [binary, "tts", model_file, "-o", out, "--seed", str(seed)]
    if voice:
        args += ["--voice", voice]
    t0 = time.time()
    subprocess.run(args + ["--", text], check=True, capture_output=True)
    took = time.time() - t0
    pcm, rate = pcm_of(open(out, "rb").read())
    return pcm, rate, took


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--sentences", required=True, help="text file, one sentence per line")
    ap.add_argument("--language", required=True)
    ap.add_argument("--voice", default="")
    ap.add_argument("--tts", required=True)
    ap.add_argument("--stt", required=True)
    ap.add_argument("--base-url", default="http://127.0.0.1:11535/v1")
    ap.add_argument("--direct", choices=["apple", "speechcpp"])
    ap.add_argument("--speechcpp", default="bin/speechcpp", help="with --direct speechcpp: the speech.cpp executable")
    ap.add_argument("--model-file", help="with --direct speechcpp: the model's GGUF file")
    ap.add_argument("--seed", type=int, default=42, help="with --direct speechcpp: the seed the manifest gives the worker")
    ap.add_argument("--out")
    args = ap.parse_args()

    sentences = [l.strip() for l in open(args.sentences, encoding="utf-8") if l.strip()]
    norm = EnglishTextNormalizer() if args.language == "en" else BasicTextNormalizer()
    tmp = tempfile.mkdtemp()
    speak(args.base_url, args.tts, args.voice, args.language, sentences[0])  # warm up
    rows, audio_s, first_s, total_s, direct_s, direct_audio = [], 0.0, [], 0.0, 0.0, 0.0
    for text in sentences:
        wav, secs, first, total = speak(args.base_url, args.tts, args.voice, args.language, text)
        heard = transcribe(args.base_url, args.stt, args.language, wav)
        row = {"text": text, "heard": heard, "audio_s": round(secs, 2), "ms": round(total * 1000)}
        audio_s += secs
        first_s.append(first)
        total_s += total
        if args.direct == "apple":
            dsecs, dtook = say_direct(args.voice, text, tmp)
            direct_s += dtook
            direct_audio += dsecs
            row["direct_ms"] = round(dtook * 1000)
        elif args.direct == "speechcpp":
            dpcm, drate, dtook = speechcpp_direct(args.speechcpp, args.model_file, args.voice, args.seed, text, tmp)
            # Oaudio streams its WAVE with a 44-byte header written before the
            # length is known, so its samples are taken by position.
            opcm = wav[44:]
            direct_s += dtook
            direct_audio += len(dpcm) / 2 / drate
            row["direct_ms"] = round(dtook * 1000)
            row["identical"] = opcm == dpcm
            if not row["identical"]:
                row["direct_audio_s"] = round(len(dpcm) / 2 / drate, 2)
        rows.append(row)

    refs, hyps = [norm(r["text"]) for r in rows], [norm(r["heard"]) for r in rows]
    first_s.sort()
    report = {
        "tts": args.tts, "stt": args.stt, "voice": args.voice, "language": args.language, "sentences": len(rows),
        "audio_seconds": round(audio_s, 1),
        "round_trip_wer": round(jiwer.wer(refs, hyps), 4),
        "round_trip_cer": round(jiwer.cer([r.replace(" ", "") for r in refs], [h.replace(" ", "") for h in hyps]), 4),
        "exact_sentences": f"{sum(r == h for r, h in zip(refs, hyps))}/{len(rows)}",
        "rtf": round(total_s / audio_s, 4),
        "first_audio_ms_median": round(1000 * first_s[len(first_s) // 2]),
        "ms_per_sentence": round(1000 * total_s / len(rows), 1),
    }
    if args.direct:
        report["direct_ms_per_sentence"] = round(1000 * direct_s / len(rows), 1)
        report["overhead_ms_per_sentence"] = round(1000 * (total_s - direct_s) / len(rows), 1)
        report["audio_seconds_direct"] = round(direct_audio, 1)
    if args.direct == "speechcpp":
        report["identical_to_direct"] = f"{sum(r['identical'] for r in rows)}/{len(rows)}"
    print(json.dumps(report, indent=2, ensure_ascii=False))
    if args.out:
        report["rows"] = rows
        json.dump(report, open(args.out, "w", encoding="utf-8"), indent=1, ensure_ascii=False)


if __name__ == "__main__":
    main()

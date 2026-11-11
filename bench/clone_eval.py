"""Check voice cloning: does the cloned speech sound like the speaker it was
cloned from, and does it say the right words?

For each of N LibriSpeech speakers: one utterance becomes a custom voice,
the cloning model speaks a sentence that speaker never said, and a speaker
verification model (not the one used anywhere inside Oaudio) compares the
result with a *different* recording of the same speaker and with recordings
of other speakers. Cloning works if the same-speaker similarity is clearly
higher. The clone is also transcribed and scored against the text.

  uv run --with jiwer --with whisper-normalizer --with sherpa-onnx --with numpy \
      python bench/clone_eval.py --root LibriSpeech/test-clean --speakers 20 \
      --tts pocket-tts --stt whisper:base.en-q5_1 --verifier wespeaker_en_voxceleb_resnet34.onnx

For another language, --dataset tsv reads <root>/test.tsv with lines of
speaker, audio path relative to root, and transcript (Zeroth-Korean unpacked
this way has ten speakers), and --language names the language spoken.
"""
import argparse, glob, io, json, os, random, subprocess, sys, time, uuid, urllib.request, wave

import jiwer, numpy as np, sherpa_onnx
from whisper_normalizer.basic import BasicTextNormalizer
from whisper_normalizer.english import EnglishTextNormalizer


def http(method, url, data=None, headers=None):
    req = urllib.request.Request(url, data=data, headers=headers or {}, method=method)
    with urllib.request.urlopen(req, timeout=600) as r:
        return r.read()


def multipart(fields, file_field, name, blob):
    b = uuid.uuid4().hex
    parts = [f'--{b}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode() for k, v in fields.items() if v]
    parts.append(f'--{b}\r\nContent-Disposition: form-data; name="{file_field}"; filename="{name}"\r\n\r\n'.encode() + blob + b"\r\n")
    parts.append(f"--{b}--\r\n".encode())
    return b"".join(parts), {"Content-Type": f"multipart/form-data; boundary={b}"}


def load(path_or_bytes):
    """Any audio -> mono float32 at 16 kHz."""
    args = ["ffmpeg", "-v", "error", "-i", "-" if isinstance(path_or_bytes, bytes) else path_or_bytes, "-ac", "1", "-ar", "16000", "-f", "s16le", "-"]
    out = subprocess.run(args, input=path_or_bytes if isinstance(path_or_bytes, bytes) else None, capture_output=True, check=True).stdout
    return np.frombuffer(out, dtype=np.int16).astype(np.float32) / 32768


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", required=True)
    ap.add_argument("--dataset", choices=["librispeech", "tsv"], default="librispeech")
    ap.add_argument("--language", default="en")
    ap.add_argument("--speakers", type=int, default=20)
    ap.add_argument("--sentences", type=int, default=1, help="sentences spoken in each cloned voice")
    ap.add_argument("--no-transcript", action="store_true", help="create the voices without saying what the reference says")
    ap.add_argument("--tts", required=True)
    ap.add_argument("--stt", required=True)
    ap.add_argument("--verifier", required=True, help="speaker-embedding ONNX model used only for scoring")
    ap.add_argument("--base-url", default="http://127.0.0.1:11535/v1")
    ap.add_argument("--out")
    args = ap.parse_args()
    rng = random.Random(11)
    english = args.language == "en"
    base_norm = EnglishTextNormalizer() if english else BasicTextNormalizer()
    norm = lambda text: " ".join(base_norm(text).split())

    extractor = sherpa_onnx.SpeakerEmbeddingExtractor(sherpa_onnx.SpeakerEmbeddingExtractorConfig(model=args.verifier, num_threads=4))
    def embed(samples):
        s = extractor.create_stream()
        s.accept_waveform(16000, samples)
        s.input_finished()
        e = np.array(extractor.compute(s))
        return e / np.linalg.norm(e)

    by_speaker = {}
    if args.dataset == "tsv":
        for line in open(os.path.join(args.root, "test.tsv"), encoding="utf-8"):
            spk, path, text = line.rstrip("\n").split("\t")
            by_speaker.setdefault(spk, []).append((os.path.join(args.root, path), text))
    else:
        for trans in sorted(glob.glob(os.path.join(args.root, "*", "*", "*.trans.txt"))):
            for line in open(trans, encoding="utf-8"):
                utt, text = line.strip().split(" ", 1)
                by_speaker.setdefault(utt.split("-")[0], []).append((os.path.join(os.path.dirname(trans), utt + ".flac"), text))
    speakers = rng.sample(sorted(by_speaker), min(args.speakers, len(by_speaker)))
    def dur(p):
        return float(subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", p], capture_output=True, text=True).stdout)

    # A held-out recording per speaker, to compare clones against.
    held = {}
    prompts = {}
    prompt_emb = {}
    for spk in speakers:
        utts = [u for u in by_speaker[spk] if 5 <= dur(u[0]) <= 12]
        rng.shuffle(utts)
        prompts[spk], held[spk] = utts[0], embed(load(utts[1][0]))
    # Sentences nobody in the cast says: transcripts of other speakers.
    # When every speaker is in the cast, each one gets a sentence spoken only
    # by somebody else.
    others = [s for s in sorted(by_speaker) if s not in speakers] or speakers
    shape = (lambda t: t[0].upper() + t[1:].lower() + ".") if english else (lambda t: t.strip() + ".")
    pool = [(s, t) for s in others for _, t in by_speaker[s][:3] if 8 <= len(t.split()) <= 18]
    def sentence_for(i, spk):
        own = {t for _, t in by_speaker[spk]}
        usable = [t for s, t in pool if s != spk and t not in own]
        return shape(usable[i % len(usable)])

    rows, same, other, refs, hyps, real_same = [], [], [], [], [], []
    real_refs, real_hyps = [], []
    synth_s = audio_s = 0.0
    for i, spk in enumerate(speakers):
        path, text = prompts[spk]
        name = f"bench-{spk}"
        try:
            http("DELETE", f"{args.base_url}/audio/voices/{name}")
        except Exception:
            pass
        body, headers = multipart({"name": name, "consent": "Reader in a public speech corpus; benchmark only, audio not kept",
                                   "transcript": "" if args.no_transcript else text.lower()}, "audio_sample", "ref.flac", open(path, "rb").read())
        voice = json.loads(http("POST", f"{args.base_url}/audio/voices", body, headers))
        # How similar two real recordings of this speaker are: the ceiling.
        prompt_emb[spk] = embed(load(path))
        real_same.append(float(prompt_emb[spk] @ held[spk]))
        # The same recognizer on a real recording of this speaker: the error
        # that is the recognizer's and the reference text's, not the clone's.
        body, headers = multipart({"model": args.stt, "language": args.language}, "file", "r.flac", open(path, "rb").read())
        real_refs.append(norm(text))
        real_hyps.append(norm(json.loads(http("POST", f"{args.base_url}/audio/transcriptions", body, headers))["text"]))
        for j in range(args.sentences):
            sentence = sentence_for(i * args.sentences + j, spk)
            t0 = time.time()
            wav = http("POST", f"{args.base_url}/audio/speech",
                       json.dumps({"model": args.tts, "voice": {"id": voice["id"]}, "input": sentence, "language": args.language,
                                   "response_format": "wav"}).encode(), {"Content-Type": "application/json"})
            took = time.time() - t0
            clone = load(wav)
            synth_s += took
            audio_s += len(clone) / 16000
            e = embed(clone)
            s_same = float(e @ held[spk])
            s_other = float(np.mean([e @ held[o] for o in speakers if o != spk]))
            same.append(s_same)
            other.append(s_other)
            body, headers = multipart({"model": args.stt, "language": args.language}, "file", "c.wav", wav)
            heard = json.loads(http("POST", f"{args.base_url}/audio/transcriptions", body, headers))["text"]
            refs.append(norm(sentence))
            hyps.append(norm(heard))
            # Which speaker in the cast does the clone resemble most?
            nearest = max(speakers, key=lambda o: float(e @ held[o]))
            rows.append({"speaker": spk, "same": round(s_same, 3), "others_mean": round(s_other, 3), "nearest_is_source": nearest == spk,
                         "seconds": round(took, 2), "text": sentence, "heard": heard})
        http("DELETE", f"{args.base_url}/audio/voices/{voice['id']}")

    report = {
        "tts": args.tts, "stt": args.stt, "language": args.language, "speakers": len(speakers), "clones": len(rows), "reference_transcript": not args.no_transcript, "verifier": os.path.basename(args.verifier),
        "similarity_clone_vs_same_speaker": round(float(np.mean(same)), 3),
        "similarity_clone_vs_other_speakers": round(float(np.mean(other)), 3),
        "similarity_real_vs_real_same_speaker": round(float(np.mean(real_same)), 3),
        "similarity_real_vs_real_other_speakers": round(float(np.mean([prompt_emb[a] @ held[b] for a in speakers for b in speakers if a != b])), 3),
        "real_recording_closest_to_its_own_speaker": f"{sum(max(speakers, key=lambda o: float(prompt_emb[a] @ held[o])) == a for a in speakers)}/{len(speakers)}",
        "clone_closest_to_its_source_speaker": f"{sum(r['nearest_is_source'] for r in rows)}/{len(rows)}",
        "wer_of_cloned_speech": round(jiwer.wer(refs, hyps), 4),
        "cer_of_cloned_speech": round(jiwer.cer([r.replace(" ", "") for r in refs], [h.replace(" ", "") for h in hyps]), 4),
        "wer_of_real_speech": round(jiwer.wer(real_refs, real_hyps), 4),
        "cer_of_real_speech": round(jiwer.cer([r.replace(" ", "") for r in real_refs], [h.replace(" ", "") for h in real_hyps]), 4),
        "rtf": round(synth_s / audio_s, 3), "ms_per_sentence": round(1000 * synth_s / len(rows)),
    }
    print(json.dumps(report, indent=2))
    if args.out:
        report["rows"] = rows
        json.dump(report, open(args.out, "w", encoding="utf-8"), indent=1, ensure_ascii=False)


if __name__ == "__main__":
    main()

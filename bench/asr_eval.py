"""Measure a speech-to-text model on a public test set, through Oaudio and,
optionally, by calling the engine directly.

The point is not to rank models. It is to show that putting Oaudio in front of
an engine changes neither what it recognises nor, by much, how long it takes:
the same audio is sent through the HTTP API (upload, decode, resample, runner)
and straight into the engine, and the two transcripts are scored the same way.

Scoring uses jiwer with the text normalizers published with Whisper, which is
what most published WER figures use.

  uv run --with jiwer --with whisper-normalizer python bench/asr_eval.py \
      --dataset librispeech --root /data/LibriSpeech/test-clean --language en \
      --model apple-speech --limit 200 --direct apple --out results.json

Datasets:
  librispeech  --root is a LibriSpeech split directory (*.flac + *.trans.txt)
  fleurs       --root holds test.tsv and the extracted audio (FLEURS layout)
"""
import argparse, glob, io, json, os, subprocess, sys, tempfile, time, uuid, urllib.request, wave

import jiwer
from whisper_normalizer.basic import BasicTextNormalizer
from whisper_normalizer.english import EnglishTextNormalizer


def load_librispeech(root):
    items = []
    for trans in sorted(glob.glob(os.path.join(root, "*", "*", "*.trans.txt"))):
        for line in open(trans, encoding="utf-8"):
            utt, text = line.strip().split(" ", 1)
            path = os.path.join(os.path.dirname(trans), utt + ".flac")
            if not os.path.exists(path):  # a copy of the set converted to WAV
                path = path[:-5] + ".wav"
            items.append((path, text))
    return items


def load_fleurs(root):
    wavs = {os.path.basename(p): p for p in glob.glob(os.path.join(root, "**", "*.wav"), recursive=True)}
    items = []
    for line in open(os.path.join(root, "test.tsv"), encoding="utf-8"):
        cols = line.rstrip("\n").split("\t")
        if cols[1] in wavs:
            items.append((wavs[cols[1]], cols[2]))  # raw transcription
    return sorted(items)


def is_pcm16(path):
    """True for a 16-bit, 16 kHz, mono WAV file."""
    try:
        with wave.open(path, "rb") as w:
            return (w.getnchannels(), w.getsampwidth(), w.getframerate(), w.getcomptype()) == (1, 2, 16000, "NONE")
    except (wave.Error, EOFError):
        return False


def duration(path):
    if is_pcm16(path):
        with wave.open(path, "rb") as w:
            return w.getnframes() / 16000
    out = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path],
                         capture_output=True, text=True)
    return float(out.stdout.strip())


def via_api(base, model, language, path):
    boundary = uuid.uuid4().hex
    parts = []
    for k, v in (("model", model), ("language", language), ("response_format", "json")):
        if not v:
            continue
        parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())
    parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{os.path.basename(path)}"\r\n'
                 f"Content-Type: application/octet-stream\r\n\r\n".encode() + open(path, "rb").read() + b"\r\n")
    parts.append(f"--{boundary}--\r\n".encode())
    req = urllib.request.Request(base + "/audio/transcriptions", data=b"".join(parts),
                                 headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
    with urllib.request.urlopen(req, timeout=600) as r:
        return json.load(r)["text"]


class WhisperServerDirect:
    """whisper.cpp's own HTTP server with the model already loaded: the
    engine with nothing of Oaudio in front of it. It only reads 16 kHz WAV,
    so other formats are converted first, outside the timed section."""

    def __init__(self, url, items):
        self.url = url.rstrip("/") + "/inference"
        self.wav = {}
        tmp = tempfile.mkdtemp()
        for i, (path, _) in enumerate(items):
            out = os.path.join(tmp, f"{i}.wav")
            subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", path, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", out], check=True)
            self.wav[path] = out

    def __call__(self, language, path):
        boundary = uuid.uuid4().hex
        fields = {"response_format": "json", "beam_size": "5", "temperature": "0.0"}
        if language:
            fields["language"] = language
        parts = [f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode() for k, v in fields.items()]
        parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="a.wav"\r\n'
                     f"Content-Type: audio/wav\r\n\r\n".encode() + open(self.wav[path], "rb").read() + b"\r\n")
        parts.append(f"--{boundary}--\r\n".encode())
        req = urllib.request.Request(self.url, data=b"".join(parts),
                                     headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
        with urllib.request.urlopen(req, timeout=600) as r:
            return json.load(r)["text"]


class AppleDirect:
    """The engine with no Oaudio audio code in the way: the helper reads the
    original file itself through AVAudioFile."""

    def __init__(self, helper):
        self.p = subprocess.Popen([helper], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)

    def __call__(self, language, path):
        locale = {"en": "en-US", "ko": "ko-KR"}.get(language, language)
        self.p.stdin.write(json.dumps({"op": "transcribe", "path": path, "locale": locale}) + "\n")
        texts = []
        while True:
            msg = json.loads(self.p.stdout.readline())
            if "segment" in msg:
                texts.append(msg["segment"]["text"])
            elif "error" in msg:
                raise RuntimeError(msg["error"])
            elif msg.get("done"):
                return " ".join(texts)


class SherpaDirect:
    """sherpa-onnx's own Python package, configured from the same manifest
    Oaudio uses, on recordings shorter than the length at which the runner
    starts cutting at silences. Needs `--with sherpa-onnx --with numpy`."""

    def __init__(self, manifest, model_dir, language):
        import numpy, sherpa_onnx
        self.np = numpy
        m = json.load(open(manifest, encoding="utf-8"))
        cfg = m["engine_config"]
        f = lambda key: os.path.join(model_dir, cfg[key])
        make = sherpa_onnx.OfflineRecognizer
        family = m["family"]
        self.online = family == "online-transducer"
        self.language_option = cfg.get("language_option") == "1"
        self.tail = float(cfg.get("tail_seconds", "0.8"))
        if self.online:
            self.rec = sherpa_onnx.OnlineRecognizer.from_transducer(
                encoder=f("encoder"), decoder=f("decoder"), joiner=f("joiner"), tokens=f("tokens"),
                feature_dim=int(cfg.get("feature_dim", "80")), num_threads=8, decoding_method="greedy_search")
        elif family == "sense-voice":
            self.rec = make.from_sense_voice(model=f("model"), tokens=f("tokens"), language=language or "auto",
                                             use_itn=cfg.get("inverse_text_normalization") != "false", num_threads=8)
        elif family == "transducer":
            self.rec = make.from_transducer(encoder=f("encoder"), decoder=f("decoder"), joiner=f("joiner"), tokens=f("tokens"),
                                            model_type=cfg.get("model_type", ""), num_threads=8)
        elif family == "qwen3-asr":
            self.rec = make.from_qwen3_asr(conv_frontend=f("conv_frontend"), encoder=f("encoder"), decoder=f("decoder"),
                                           tokenizer=f("tokenizer"), num_threads=8)
        elif family == "dolphin":
            self.rec = make.from_dolphin_ctc(model=f("model"), tokens=f("tokens"), num_threads=8)
        elif family == "moonshine":
            self.rec = make.from_moonshine_v2(encoder=f("encoder"), decoder=f("merged_decoder"), tokens=f("tokens"), num_threads=8)
        else:
            raise SystemExit(f"no direct path for family {family}")

    def __call__(self, language, path):
        pcm = None
        if is_pcm16(path):
            # Read it in process, so the timing is the engine's and not a
            # converter's start-up.
            with wave.open(path, "rb") as w:
                pcm = w.readframes(w.getnframes())
        if pcm is None:
            pcm = subprocess.run(["ffmpeg", "-v", "error", "-i", path, "-ac", "1", "-ar", "16000", "-f", "s16le", "-"],
                                 capture_output=True, check=True).stdout
        samples = self.np.frombuffer(pcm, dtype=self.np.int16).astype(self.np.float32) / 32768
        if self.online:
            # The whole recording at once, then the same closing silence the
            # runner appends. The text is the tokens joined, as the runner
            # does: sherpa-onnx's own text drops the spaces between Hangul.
            stream = self.rec.create_stream()
            if self.language_option:
                stream.set_option("language", language or "auto")
            stream.accept_waveform(16000, samples)
            stream.accept_waveform(16000, self.np.zeros(int(self.tail * 16000), dtype=self.np.float32))
            stream.input_finished()
            while self.rec.is_ready(stream):
                self.rec.decode_stream(stream)
            tokens = json.loads(self.rec.get_result_as_json_string(stream))["tokens"]
            return " ".join("".join(tokens).replace("▁", " ").split())
        stream = self.rec.create_stream()
        stream.accept_waveform(16000, samples)
        self.rec.decode_stream(stream)
        return stream.result.text


class SpeechcppDirect:
    """speech.cpp's own command line, `speech asr`, on every file in one
    process: the engine with nothing of Oaudio in front of it. Results are
    computed in one batch, whose time (model load included, about a second)
    is the run's time."""

    batch = True

    def __init__(self, binary, model_file, language):
        self.cmd = [binary, "asr", model_file, "--format", "json"]
        if language:
            self.cmd += ["--language", language]

    def __call__(self, paths):
        out = subprocess.run(self.cmd + list(paths), capture_output=True, text=True, check=True).stdout
        texts = {}
        for line in out.splitlines():
            if line.strip():
                o = json.loads(line)
                texts[o["file"]] = o.get("text", "")
        return [texts[p] for p in paths]


def score(refs, hyps, language):
    norm = EnglishTextNormalizer() if language == "en" else BasicTextNormalizer()
    r = [" ".join(norm(x).split()) for x in refs]
    h = [" ".join(norm(x).split()) for x in hyps]
    keep = [i for i, x in enumerate(r) if x]
    r, h = [r[i] for i in keep], [h[i] for i in keep]
    out = {"wer": jiwer.wer(r, h)}
    out["cer"] = jiwer.cer([x.replace(" ", "") for x in r], [x.replace(" ", "") for x in h])
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dataset", choices=["librispeech", "fleurs"], required=True)
    ap.add_argument("--root", required=True)
    ap.add_argument("--language", required=True)
    ap.add_argument("--model", required=True)
    ap.add_argument("--base-url", default="http://127.0.0.1:11535/v1")
    ap.add_argument("--limit", type=int, default=200)
    ap.add_argument("--direct", choices=["apple", "whisper-server", "sherpa", "speechcpp"], help="also run the engine directly")
    ap.add_argument("--speechcpp", default="bin/speechcpp", help="for --direct speechcpp: speech.cpp's executable")
    ap.add_argument("--model-file", help="for --direct speechcpp: the model's GGUF file")
    ap.add_argument("--manifest", help="for --direct sherpa: the model's manifest")
    ap.add_argument("--model-dir", help="for --direct sherpa: the directory holding the model's files")
    ap.add_argument("--auto-language", action="store_true", help="do not tell the model the language; score as --language")
    ap.add_argument("--whisper-server", default="http://127.0.0.1:8178", help="URL of a running whisper.cpp server")
    ap.add_argument("--apple-helper", default="bin/oaudio-apple-stt")
    ap.add_argument("--pcm16", action="store_true",
                    help="convert every recording to 16-bit 16 kHz WAV first, so both paths get the same samples")
    ap.add_argument("--out")
    args = ap.parse_args()

    items = (load_librispeech if args.dataset == "librispeech" else load_fleurs)(args.root)
    # Evenly spaced sample across the whole set, so every speaker is represented.
    if args.limit and len(items) > args.limit:
        step = len(items) / args.limit
        items = [items[int(i * step)] for i in range(args.limit)]
    audio_s = sum(duration(p) for p, _ in items)
    print(f"{len(items)} utterances, {audio_s / 60:.1f} minutes of audio", file=sys.stderr)

    if args.pcm16:
        # Float or compressed originals reach the engine as 16-bit samples
        # either way, but two converters can differ by one step on a few
        # samples, and that is enough to change a word in some models.
        tmp = tempfile.mkdtemp()
        converted = []
        for i, (path, text) in enumerate(items):
            if is_pcm16(path):
                converted.append((path, text))
                continue
            out = os.path.join(tmp, f"{i}.wav")
            subprocess.run(["ffmpeg", "-v", "error", "-y", "-i", path, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", out], check=True)
            converted.append((out, text))
        names = [os.path.basename(p) for p, _ in items]
        items = converted
    else:
        names = [os.path.basename(p) for p, _ in items]

    refs = [t for _, t in items]
    hint = "" if args.auto_language else args.language
    runs = {"oaudio": lambda p: via_api(args.base_url, args.model, hint, p)}
    if args.direct == "apple":
        direct = AppleDirect(args.apple_helper)
        runs["direct"] = lambda p: direct(args.language, p)
    elif args.direct == "whisper-server":
        wdirect = WhisperServerDirect(args.whisper_server, items)
        runs["direct"] = lambda p: wdirect(args.language, p)
    elif args.direct == "sherpa":
        sdirect = SherpaDirect(args.manifest, args.model_dir, hint)
        runs["direct"] = lambda p: sdirect(hint, p)
    elif args.direct == "speechcpp":
        runs["direct"] = SpeechcppDirect(args.speechcpp, args.model_file, hint)

    report = {"dataset": args.dataset, "language": args.language, "model": args.model,
              "pcm16_input": args.pcm16, "utterances": len(items), "audio_seconds": round(audio_s, 1), "runs": {}}
    hyps = {}
    for name, fn in runs.items():
        if getattr(fn, "batch", False):
            fn([items[0][0]])  # warm up the file cache
            t0 = time.time()
            out = fn([p for p, _ in items])
            took = time.time() - t0
        else:
            fn(items[0][0])  # warm up: model load is not what is being measured
            out, t0 = [], time.time()
            for path, _ in items:
                out.append(fn(path))
            took = time.time() - t0
        hyps[name] = out
        report["runs"][name] = {**score(refs, out, args.language), "seconds": round(took, 2),
                                "rtf": round(took / audio_s, 5), "ms_per_utterance": round(1000 * took / len(items), 1)}
    if "direct" in hyps:
        norm = EnglishTextNormalizer() if args.language == "en" else BasicTextNormalizer()
        raw_norm = norm
        norm = lambda text: " ".join(raw_norm(text).split())  # ignore leading and trailing whitespace
        same = sum(norm(a) == norm(b) for a, b in zip(hyps["oaudio"], hyps["direct"]))
        report["identical_transcripts"] = f"{same}/{len(items)}"
        report["differing"] = [{"file": names[i], "oaudio": a, "direct": b}
                               for i, (a, b) in enumerate(zip(hyps["oaudio"], hyps["direct"])) if norm(a) != norm(b)][:20]
        report["overhead_ms_per_utterance"] = round(report["runs"]["oaudio"]["ms_per_utterance"] -
                                                    report["runs"]["direct"]["ms_per_utterance"], 1)
    print(json.dumps({k: v for k, v in report.items() if k != "differing"}, indent=2, ensure_ascii=False))
    if args.out:
        report["transcripts"] = [{"file": names[i], "reference": r, **{n: hyps[n][i] for n in hyps}}
                                 for i, (p, r) in enumerate(items)]
        json.dump(report, open(args.out, "w", encoding="utf-8"), indent=1, ensure_ascii=False)


if __name__ == "__main__":
    main()

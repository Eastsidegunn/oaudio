"""Score voice activity detection, diarization and speaker-labelled
transcripts on conversations with known answers (see make_mixes.py).

  uv run --with jiwer --with whisper-normalizer --with sherpa-onnx --with numpy \
      python bench/diar_eval.py --mixes mixes --stt whisper:base.en-q5_1 --direct \
      --segmentation ~/.oaudio/models/pyannote-diarize/3.0/model.onnx \
      --embedding ~/.oaudio/models/pyannote-diarize/3.0/3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx

Diarization error rate is computed on 10 ms frames with a 0.25 s collar
around each reference boundary and the best one-to-one speaker mapping, the
usual convention. --direct also runs sherpa-onnx itself, through its Python
package, to check that Oaudio returns the engine's own answer.
"""
import argparse, glob, itertools, json, os, sys, time, uuid, urllib.request, wave

import jiwer
from whisper_normalizer.english import EnglishTextNormalizer

FRAME = 0.01
COLLAR = 0.25


def post(base, path, fields, wav_path):
    boundary = uuid.uuid4().hex
    parts = [f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode() for k, v in fields.items() if v]
    parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="a.wav"\r\n\r\n'.encode()
                 + open(wav_path, "rb").read() + b"\r\n")
    parts.append(f"--{boundary}--\r\n".encode())
    req = urllib.request.Request(base + path, data=b"".join(parts), headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
    with urllib.request.urlopen(req, timeout=600) as r:
        return json.load(r)


def frames(turns, n, key):
    out = [None] * n
    for t in turns:
        for i in range(int(t["start"] / FRAME), min(n, int(t["end"] / FRAME))):
            out[i] = t[key]
    return out


def der(ref_turns, hyp_turns, duration):
    n = int(duration / FRAME)
    ref, hyp = frames(ref_turns, n, "speaker"), frames(hyp_turns, n, "speaker")
    scored = [True] * n
    for t in ref_turns:
        for edge in (t["start"], t["end"]):
            for i in range(max(0, int((edge - COLLAR) / FRAME)), min(n, int((edge + COLLAR) / FRAME))):
                scored[i] = False
    idx = [i for i in range(n) if scored[i]]
    ref_ids, hyp_ids = sorted({ref[i] for i in idx} - {None}), sorted({hyp[i] for i in idx} - {None}, key=str)
    overlap = {(r, h): 0 for r in ref_ids for h in hyp_ids}
    for i in idx:
        if ref[i] is not None and hyp[i] is not None:
            overlap[(ref[i], hyp[i])] += 1
    # Best one-to-one mapping. Speaker counts are tiny, so try them all.
    pad = hyp_ids + [None] * max(0, len(ref_ids) - len(hyp_ids))
    best = max((sum(overlap.get((r, h), 0) for r, h in zip(ref_ids, perm)) for perm in itertools.permutations(pad, len(ref_ids))), default=0)
    total = sum(1 for i in idx if ref[i] is not None)
    miss = sum(1 for i in idx if ref[i] is not None and hyp[i] is None)
    false_alarm = sum(1 for i in idx if ref[i] is None and hyp[i] is not None)
    both = sum(1 for i in idx if ref[i] is not None and hyp[i] is not None)
    return {"total": total, "miss": miss, "false_alarm": false_alarm, "confusion": both - best}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mixes", required=True)
    ap.add_argument("--base-url", default="http://127.0.0.1:11535/v1")
    ap.add_argument("--vad", default="silero-vad")
    ap.add_argument("--diarizer", default="pyannote-diarize")
    ap.add_argument("--stt", default="")
    ap.add_argument("--direct", action="store_true")
    ap.add_argument("--segmentation")
    ap.add_argument("--embedding")
    ap.add_argument("--threshold", type=float, default=0.8, help="clustering threshold for the direct run; match the manifest")
    ap.add_argument("--out")
    args = ap.parse_args()
    norm = EnglishTextNormalizer()

    sd = None
    if args.direct:
        import numpy as np, sherpa_onnx
        cfg = sherpa_onnx.OfflineSpeakerDiarizationConfig(
            segmentation=sherpa_onnx.OfflineSpeakerSegmentationModelConfig(
                pyannote=sherpa_onnx.OfflineSpeakerSegmentationPyannoteModelConfig(model=os.path.expanduser(args.segmentation)), num_threads=8),
            embedding=sherpa_onnx.SpeakerEmbeddingExtractorConfig(model=os.path.expanduser(args.embedding), num_threads=8),
            clustering=sherpa_onnx.FastClusteringConfig(num_clusters=-1, threshold=args.threshold),
            min_duration_on=0.3, min_duration_off=0.5)
        sd = sherpa_onnx.OfflineSpeakerDiarization(cfg)

    tot = {"total": 0, "miss": 0, "false_alarm": 0, "confusion": 0}
    count_ok = gaps = gaps_found = identical = n = 0
    seconds = {"vad": 0.0, "diarize": 0.0, "diarize_direct": 0.0, "audio": 0.0}
    spk_words = spk_right = 0
    refs, hyps, rows = [], [], []
    files = sorted(glob.glob(os.path.join(args.mixes, "*.json")))
    post(args.base_url, "/audio/diarizations", {"model": args.diarizer}, files[0][:-5] + ".wav")  # warm up
    post(args.base_url, "/audio/vad", {"model": args.vad}, files[0][:-5] + ".wav")
    for meta_path in files:
        meta = json.load(open(meta_path))
        wav = meta_path[:-5] + ".wav"
        n += 1
        seconds["audio"] += meta["duration"]

        t0 = time.time()
        vad = post(args.base_url, "/audio/vad", {"model": args.vad}, wav)
        seconds["vad"] += time.time() - t0
        # Each silence inserted between turns should fall between two detected spans.
        for a, b in zip(meta["turns"], meta["turns"][1:]):
            gaps += 1
            mid = (a["end"] + b["start"]) / 2
            if not any(s["start"] <= mid <= s["end"] for s in vad["segments"]):
                gaps_found += 1

        t0 = time.time()
        d = post(args.base_url, "/audio/diarizations", {"model": args.diarizer}, wav)
        seconds["diarize"] += time.time() - t0
        e = der(meta["turns"], d["segments"], meta["duration"])
        for k in tot:
            tot[k] += e[k]
        count_ok += len(d["speakers"]) == meta["speakers"]
        row = {"mix": os.path.basename(wav), "speakers": meta["speakers"], "found": len(d["speakers"]),
               "der": round((e["miss"] + e["false_alarm"] + e["confusion"]) / max(e["total"], 1), 4)}

        if sd is not None:
            with wave.open(wav) as w:
                samples = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16).astype(np.float32) / 32768
            t0 = time.time()
            res = sd.process(samples).sort_by_start_time()
            seconds["diarize_direct"] += time.time() - t0
            order, direct = {}, []
            for r in res:
                order.setdefault(r.speaker, len(order))
                direct.append((round(r.start, 2), round(r.end, 2), order[r.speaker]))
            ours = [(round(s["start"], 2), round(s["end"], 2), ord(s["speaker"]) - 65) for s in d["segments"]]
            identical += ours == direct
            row["identical_to_direct"] = ours == direct

        if args.stt:
            tr = post(args.base_url, "/audio/transcriptions",
                      {"model": args.stt, "response_format": "diarized_json", "diarization_model": args.diarizer, "language": "en"}, wav)
            refs.append(norm(" ".join(t["text"] for t in meta["turns"])))
            hyps.append(norm(tr["text"]))
            # Word-level speaker accuracy: label each hypothesis word with the
            # reference speaker talking at that moment, via the label mapping
            # that agrees most often.
            pairs = {}
            for seg in tr["segments"]:
                mid = (seg["start"] + seg["end"]) / 2
                who = next((t["speaker"] for t in meta["turns"] if t["start"] - 0.3 <= mid <= t["end"] + 0.3), None)
                words = len(seg["text"].split())
                if who is not None:
                    pairs[(seg["speaker"], who)] = pairs.get((seg["speaker"], who), 0) + words
                spk_words += words
            labels = sorted({p[0] for p in pairs})
            people = sorted({p[1] for p in pairs})
            pad = people + [None] * max(0, len(labels) - len(people))
            spk_right += max((sum(pairs.get((l, w), 0) for l, w in zip(labels, perm)) for perm in itertools.permutations(pad, len(labels))), default=0)
        rows.append(row)

    report = {
        "conversations": n, "audio_minutes": round(seconds["audio"] / 60, 1),
        "vad": {"model": args.vad, "inserted_silences_detected": f"{gaps_found}/{gaps}", "rtf": round(seconds["vad"] / seconds["audio"], 5)},
        "diarization": {
            "model": args.diarizer,
            "der": round((tot["miss"] + tot["false_alarm"] + tot["confusion"]) / tot["total"], 4),
            "missed_speech": round(tot["miss"] / tot["total"], 4), "false_alarm": round(tot["false_alarm"] / tot["total"], 4),
            "speaker_confusion": round(tot["confusion"] / tot["total"], 4),
            "speaker_count_correct": f"{count_ok}/{n}", "rtf": round(seconds["diarize"] / seconds["audio"], 5),
        },
    }
    if sd is not None:
        report["diarization"]["identical_to_engine_direct"] = f"{identical}/{n}"
        report["diarization"]["ms_per_conversation"] = round(1000 * seconds["diarize"] / n, 1)
        report["diarization"]["direct_ms_per_conversation"] = round(1000 * seconds["diarize_direct"] / n, 1)
    if args.stt:
        report["diarized_transcript"] = {"stt": args.stt, "wer": round(jiwer.wer(refs, hyps), 4),
                                         "words_attributed_to_right_speaker": round(spk_right / max(spk_words, 1), 4)}
    print(json.dumps(report, indent=2))
    if args.out:
        report["rows"] = rows
        json.dump(report, open(args.out, "w"), indent=1)


if __name__ == "__main__":
    main()

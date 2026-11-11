"""Build multi-speaker conversations with known answers from LibriSpeech.

Each conversation takes 2 to 4 real speakers from a LibriSpeech split and
alternates their utterances with a short silence between turns. Because the
pieces come from labelled recordings, who spoke when and what was said are
known exactly, which is what diarization and realtime turn detection are
scored against.

There is no overlapping speech, so this is an easy case: a floor on what a
diarizer should manage, not a substitute for a meeting corpus.

  python bench/make_mixes.py --root LibriSpeech/test-clean --out mixes --count 30
"""
import argparse, glob, json, os, random, subprocess, wave

ap = argparse.ArgumentParser()
ap.add_argument("--root", required=True)
ap.add_argument("--out", required=True)
ap.add_argument("--count", type=int, default=30)
ap.add_argument("--gap", type=float, default=0.6)
ap.add_argument("--seed", type=int, default=7)
args = ap.parse_args()

rng = random.Random(args.seed)
by_speaker = {}
for trans in sorted(glob.glob(os.path.join(args.root, "*", "*", "*.trans.txt"))):
    for line in open(trans, encoding="utf-8"):
        utt, text = line.strip().split(" ", 1)
        if 4 <= len(text.split()) <= 22:
            by_speaker.setdefault(utt.split("-")[0], []).append((os.path.join(os.path.dirname(trans), utt + ".flac"), text))
speakers = sorted(by_speaker)
os.makedirs(args.out, exist_ok=True)

def pcm(path):
    return subprocess.run(["ffmpeg", "-v", "error", "-i", path, "-ac", "1", "-ar", "16000", "-f", "s16le", "-"],
                          capture_output=True, check=True).stdout

for n in range(args.count):
    k = 2 + n % 3
    cast = rng.sample(speakers, k)
    turns_n = rng.randint(6, 8)
    order, last = [], None
    for _ in range(turns_n):
        who = rng.choice([s for s in cast if s != last])
        order.append(who)
        last = who
    for s in cast:  # everyone speaks at least once
        if s not in order:
            order[rng.randrange(len(order))] = s
    audio, t, turns, used = b"", 0.0, [], set()
    for who in order:
        path, text = rng.choice([u for u in by_speaker[who] if u[0] not in used])
        used.add(path)
        data = pcm(path)
        dur = len(data) / 2 / 16000
        turns.append({"speaker": who, "start": round(t, 3), "end": round(t + dur, 3), "text": text, "source": os.path.basename(path)})
        audio += data + b"\x00" * int(args.gap * 16000) * 2
        t += dur + args.gap
    name = os.path.join(args.out, f"mix{n:02d}")
    with wave.open(name + ".wav", "wb") as w:
        w.setnchannels(1); w.setsampwidth(2); w.setframerate(16000); w.writeframes(audio)
    json.dump({"speakers": k, "duration": round(t, 3), "turns": turns}, open(name + ".json", "w"), indent=1)
print(f"wrote {args.count} conversations to {args.out}")

"""Drive Oaudio's realtime transcription with the unmodified OpenAI SDK.

Streams a recording into a Realtime transcription session the way a
microphone would, 100 ms at a time, and prints what the server detects.

  uv run --with "openai[realtime]" python bench/realtime_sdk.py talk.wav \
      --model whisper --expect-turns 4
"""
import argparse, base64, json, subprocess, sys, time

from openai import OpenAI

ap = argparse.ArgumentParser()
ap.add_argument("audio")
ap.add_argument("--model", required=True)
ap.add_argument("--base-url", default="http://127.0.0.1:11535/v1")
ap.add_argument("--language", default="")
ap.add_argument("--speed", type=float, default=1.0, help="1 = real time; higher sends faster")
ap.add_argument("--silence-ms", type=int, default=500)
ap.add_argument("--expect-turns", type=int, default=0)
ap.add_argument("--out")
args = ap.parse_args()

# The Realtime API takes 24 kHz 16-bit mono PCM.
pcm = subprocess.run(["ffmpeg", "-v", "error", "-i", args.audio, "-ac", "1", "-ar", "24000", "-f", "s16le", "-"],
                     capture_output=True, check=True).stdout
pcm += b"\x00" * 24000 * 2 * 2  # two seconds of silence so the last utterance ends
chunk = 24000 * 2 // 10

client = OpenAI(base_url=args.base_url, api_key="oaudio")
events, turns = [], []
with client.realtime.connect(model=args.model) as conn:
    first = conn.recv()
    assert first.type == "session.created", first.type
    transcription = {"model": args.model}
    if args.language:
        transcription["language"] = args.language
    conn.session.update(session={"type": "transcription", "audio": {"input": {
        "format": {"type": "audio/pcm", "rate": 24000},
        "transcription": transcription,
        "turn_detection": {"type": "server_vad", "silence_duration_ms": args.silence_ms},
    }}})
    updated = conn.recv()
    assert updated.type == "session.updated", updated.type

    start = time.time()
    sent_at = {}  # audio ms -> wall clock when that audio had been sent
    def audio_clock():
        return (time.time() - start) * args.speed

    import threading
    def pump():
        for i in range(0, len(pcm), chunk):
            due = i / (24000 * 2) / args.speed
            delay = due - (time.time() - start)
            if delay > 0:
                time.sleep(delay)
            conn.input_audio_buffer.append(audio=base64.b64encode(pcm[i:i + chunk]).decode())
        # Everything is sent. Clearing the (by now silent) buffer makes the
        # server answer, which wakes the receive loop below so it can finish.
        time.sleep(1.5 / args.speed)
        conn.input_audio_buffer.clear()
    t = threading.Thread(target=pump)
    t.start()

    stopped_at, done, drained = {}, 0, False
    started_ms, deltas = {}, {}  # item -> audio_start_ms; item -> [(audio clock s, text)]
    deadline = time.time() + len(pcm) / (24000 * 2) / args.speed + 20
    while time.time() < deadline and not (drained and done >= len(stopped_at)):
        ev = conn.recv()
        now = time.time() - start
        events.append(ev.type)
        if ev.type == "input_audio_buffer.speech_started":
            started_ms[ev.item_id] = ev.audio_start_ms
            print(f"[{now:6.2f}s] speech started at {ev.audio_start_ms} ms")
        elif ev.type == "conversation.item.input_audio_transcription.delta":
            deltas.setdefault(ev.item_id, []).append((now * args.speed, ev.delta))
            if ev.item_id not in stopped_at:
                print(f"[{now:6.2f}s]   while speaking: {ev.delta!r}")
        elif ev.type == "input_audio_buffer.speech_stopped":
            stopped_at[ev.item_id] = (now, ev.audio_end_ms)
            print(f"[{now:6.2f}s] speech stopped at {ev.audio_end_ms} ms")
        elif ev.type == "conversation.item.input_audio_transcription.completed":
            wall, end_ms = stopped_at.get(ev.item_id, (now, 0))
            # How long after the speaker stopped (in the audio's own time) the transcript arrived.
            lag = now * args.speed - end_ms / 1000
            ds = deltas.get(ev.item_id, [])
            begin = started_ms.get(ev.item_id, 0) / 1000
            # In the audio's own time: when the first words arrived after the
            # speaker began, how many arrived before the speaker stopped, and
            # how often the transcript grew while they were talking.
            early = [t for t, _ in ds if t < end_ms / 1000]
            gaps = [b - a for a, b in zip(early, early[1:])]
            turns.append({"transcript": ev.transcript.strip(), "start_ms": started_ms.get(ev.item_id), "end_ms": end_ms,
                          "lag_s": round(lag, 2), "transcribe_s": round(now - wall, 2),
                          "first_delta_after_onset_s": round(ds[0][0] - begin, 2) if ds else None,
                          "deltas": len(ds), "deltas_while_speaking": len(early),
                          "chars_while_speaking": sum(len(d) for t, d in ds if t < end_ms / 1000),
                          "median_delta_interval_s": round(sorted(gaps)[len(gaps) // 2], 2) if gaps else None,
                          "deltas_add_up": "".join(d for _, d in ds).strip() == ev.transcript.strip()})
            print(f"[{now:6.2f}s] transcript: {ev.transcript.strip()}")
            done += 1
        elif ev.type == "input_audio_buffer.cleared":
            drained = True
        elif ev.type == "error":
            print("ERROR", ev.error.message)
            sys.exit(1)
    t.join()

kinds = set(events)
needed = {"input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped", "input_audio_buffer.committed",
          "conversation.item.input_audio_transcription.delta", "conversation.item.input_audio_transcription.completed"}
ok = needed <= kinds and (not args.expect_turns or len(turns) == args.expect_turns)
lags = sorted(x["lag_s"] for x in turns)
med = lambda xs: sorted(xs)[len(xs) // 2] if xs else None
report = {"model": args.model, "turns": len(turns), "event_types": sorted(kinds), "missing_event_types": sorted(needed - kinds),
          "median_lag_after_speech_end_s": lags[len(lags) // 2] if lags else None,
          "median_first_delta_after_onset_s": med([x["first_delta_after_onset_s"] for x in turns if x["first_delta_after_onset_s"] is not None]),
          "turns_with_words_while_speaking": sum(x["deltas_while_speaking"] > 0 for x in turns),
          "median_delta_interval_while_speaking_s": med([x["median_delta_interval_s"] for x in turns if x["median_delta_interval_s"]]),
          "share_of_text_while_speaking": round(sum(x["chars_while_speaking"] for x in turns) /
                                                max(1, sum(len(x["transcript"]) for x in turns)), 2),
          "deltas_add_up_to_transcript": sum(x["deltas_add_up"] for x in turns),
          "silence_ms": args.silence_ms, "speed": args.speed, "results": turns}
print(json.dumps({k: v for k, v in report.items() if k != "results"}, indent=2))
if args.out:
    json.dump(report, open(args.out, "w"), indent=1)
print("PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)

package server

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// With a streaming model, words reach the client while the utterance is
// still being spoken: deltas for an item come before its speech_stopped, add
// up to the final transcript, and the item's events keep their order.
func TestRealtimeStreaming(t *testing.T) {
	ts := newTestServer(t)
	c := dialWS(t, ts, "/v1/realtime?model=ref-stream")
	if ev := c.recv(t); ev["type"] != "session.created" {
		t.Fatalf("first event: %v", ev)
	}
	c.send(t, map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription",
		"audio": map[string]any{"input": map[string]any{"turn_detection": map[string]any{"type": "server_vad", "silence_duration_ms": 300}}}}})
	c.until(t, "session.updated")

	// Two utterances of eight words (1.3 s each), a second of silence after
	// each, sent at the pace of speech so that detection sees them grow.
	utter := body(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "a b c d e f g h", "response_format": "pcm"}))
	stream := append(append(append([]byte{}, utter...), make([]byte, 24000*2)...), utter...)
	stream = append(stream, make([]byte, 24000*2)...)
	sent := make(chan error, 1)
	go func() {
		defer close(sent)
		for i := 0; i < len(stream); i += 4800 {
			end := min(i+4800, len(stream))
			if err := c.write(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(stream[i:end])}); err != nil {
				sent <- err
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	type itemLog struct {
		kinds  []string
		deltas string
		final  any
	}
	items := map[string]*itemLog{}
	var order []string
	for done := 0; done < 2; {
		ev := c.recv(t)
		kind, _ := ev["type"].(string)
		if kind == "error" || kind == "conversation.item.input_audio_transcription.failed" {
			t.Fatalf("%s: %v", kind, ev)
		}
		id, _ := ev["item_id"].(string)
		if item, ok := ev["item"].(map[string]any); ok {
			id, _ = item["id"].(string)
		}
		if id == "" {
			continue
		}
		l := items[id]
		if l == nil {
			l = &itemLog{}
			items[id] = l
			order = append(order, id)
		}
		l.kinds = append(l.kinds, kind)
		switch kind {
		case "conversation.item.input_audio_transcription.delta":
			l.deltas += ev["delta"].(string)
		case "conversation.item.input_audio_transcription.completed":
			l.final = ev["transcript"]
			done++
		}
	}
	if err := <-sent; err != nil {
		t.Fatalf("streaming audio: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("got %d items, want 2", len(order))
	}
	for _, id := range order {
		l := items[id]
		want := "word1 word2 word3 word4 word5 word6 word7 word8"
		if l.final != want || l.deltas != want {
			t.Errorf("transcript %q from deltas %q, want %q", l.final, l.deltas, want)
		}
		seq := strings.Join(l.kinds, " ")
		stopped := strings.Index(seq, "input_audio_buffer.speech_stopped")
		firstDelta := strings.Index(seq, "transcription.delta")
		if firstDelta < 0 || stopped < 0 || firstDelta > stopped {
			t.Errorf("no delta before speech_stopped: %s", seq)
		}
		if l.kinds[0] != "input_audio_buffer.speech_started" || l.kinds[len(l.kinds)-1] != "conversation.item.input_audio_transcription.completed" {
			t.Errorf("item events out of order: %s", seq)
		}
		if a, b := strings.Index(seq, "speech_stopped"), strings.Index(seq, "input_audio_buffer.committed"); a > b {
			t.Errorf("committed before speech_stopped: %s", seq)
		}
	}

	// Turned off, turn detection leaves the end to the client, and a model
	// that streams still transcribes what it is given on commit.
	c.send(t, map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription",
		"audio": map[string]any{"input": map[string]any{"turn_detection": nil}}}})
	c.until(t, "session.updated")
	c.send(t, map[string]any{"type": "input_audio_buffer.clear"})
	c.until(t, "input_audio_buffer.cleared")
	c.send(t, map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(utter)})
	c.send(t, map[string]any{"type": "input_audio_buffer.commit"})
	if _, done := c.until(t, "conversation.item.input_audio_transcription.completed"); done["transcript"] != "word1 word2 word3 word4 word5 word6 word7 word8" {
		t.Errorf("manual commit: transcript %q", done["transcript"])
	}
}

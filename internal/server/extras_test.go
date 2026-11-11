package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eastsidegunn/oaudio/internal/audio"
	"github.com/eastsidegunn/oaudio/runner"
)

func postForm(t *testing.T, ts *httptest.Server, path, fileField string, file []byte, fields ...string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i+1 < len(fields); i += 2 {
		mw.WriteField(fields[i], fields[i+1])
	}
	if file != nil {
		part, _ := mw.CreateFormFile(fileField, "audio.wav")
		part.Write(file)
	}
	mw.Close()
	resp, err := http.Post(ts.URL+path, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// conversation makes audio in which the low and high voices of the reference
// engine take turns: low says two words, high three, low one.
func conversation(t *testing.T, ts *httptest.Server) []byte {
	t.Helper()
	var pcm []byte
	for _, turn := range [][2]string{{"low", "a b"}, {"high", "c d e"}, {"low", "f"}} {
		pcm = append(pcm, body(t, speech(t, ts, map[string]any{"model": "ref-tts", "voice": turn[0], "input": turn[1], "response_format": "pcm"}))...)
	}
	return audio.EncodeWAV(pcm, 24000)
}

func TestVADEndpoint(t *testing.T) {
	ts := newTestServer(t)
	wav := body(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "one two three", "response_format": "wav"}))
	// The model is implied: only one vad model is installed.
	resp := postForm(t, ts, "/v1/audio/vad", "file", wav)
	var out struct {
		Duration, SpeechDuration float64 `json:"-"`
		Segments                 []runner.Span
	}
	raw := body(t, resp)
	var top map[string]any
	json.Unmarshal(raw, &top)
	if json.Unmarshal(raw, &out) != nil || resp.StatusCode != 200 || len(out.Segments) != 3 {
		t.Fatalf("vad: %d %s", resp.StatusCode, raw)
	}
	if sd := top["speech_duration"].(float64); sd < 0.29 || sd > 0.31 {
		t.Errorf("speech_duration %.3f, want 0.30 (three 100 ms bursts)", sd)
	}
	// A longer minimum silence joins the bursts into one span.
	resp = postForm(t, ts, "/v1/audio/vad", "file", wav, "model", "ref-vad", "min_silence_ms", "200")
	if json.Unmarshal(body(t, resp), &out); len(out.Segments) != 1 {
		t.Errorf("min_silence_ms=200 gave %d spans, want 1", len(out.Segments))
	}
	wantError(t, postForm(t, ts, "/v1/audio/vad", "file", wav, "model", "ref-stt"), 400, "does not detect speech")
	wantError(t, postForm(t, ts, "/v1/audio/vad", "file", wav, "threshold", "7"), 400, "threshold")
}

func TestDiarization(t *testing.T) {
	ts := newTestServer(t)
	wav := conversation(t, ts)

	resp := postForm(t, ts, "/v1/audio/diarizations", "file", wav)
	var d struct {
		Speakers []string
		Segments []struct {
			Start, End float64
			Speaker    string
		}
	}
	raw := body(t, resp)
	if json.Unmarshal(raw, &d) != nil || fmt.Sprint(d.Speakers) != "[A B]" || len(d.Segments) != 3 ||
		d.Segments[0].Speaker != "A" || d.Segments[1].Speaker != "B" || d.Segments[2].Speaker != "A" {
		t.Fatalf("diarization: %s", raw)
	}

	// The transcript combined with speakers, in OpenAI's diarized_json shape.
	// Whisper-style recognizers emit one segment per phrase; the reference
	// engine emits one per word with word times, so words regroup by speaker.
	resp = transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt", "response_format", "diarized_json")
	var out struct {
		Task     string
		Duration float64
		Text     string
		Segments []struct {
			Type, ID, Speaker, Text string
			Start, End              float64
		}
	}
	raw = body(t, resp)
	if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("diarized_json: %d %s", resp.StatusCode, raw)
	}
	got := ""
	for _, s := range out.Segments {
		got += s.Speaker + ":" + s.Text + "|"
		if s.Type != "transcript.text.segment" || s.ID == "" || s.End <= s.Start {
			t.Errorf("bad segment %+v", s)
		}
	}
	if got != "A:word1 word2|B:word3 word4 word5|A:word6|" || out.Task != "transcribe" || out.Text != "word1 word2 word3 word4 word5 word6" {
		t.Errorf("diarized transcript %q (text %q)", got, out.Text)
	}
	wantError(t, transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt", "response_format", "diarized_json", "known_speaker_names[]", "x"), 400, "known_speaker_names")
	wantError(t, transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt", "response_format", "diarized_json", "diarization_model", "ref-tts"), 400, "tell speakers apart")
}

func TestAssignSpeakers(t *testing.T) {
	turns := []runner.Turn{{Start: 0, End: 2, Speaker: 0}, {Start: 2.5, End: 5, Speaker: 1}}
	// Without word times a segment goes to whoever speaks for most of it.
	segs := AssignSpeakers([]runner.Segment{{Start: 0, End: 1.8, Text: " Hello there."}, {Start: 1.8, End: 5, Text: " Hi."}}, turns)
	if len(segs) != 2 || segs[0].Speaker != 0 || segs[1].Speaker != 1 {
		t.Errorf("segment-level: %+v", segs)
	}
	// With word times a segment spanning the change is split there, and a
	// word in the gap between turns goes to the nearer turn.
	words := []runner.Word{{Word: "one", Start: 0.1, End: 0.5}, {Word: "two", Start: 1.9, End: 2.4}, {Word: "three", Start: 2.4, End: 3}}
	segs = AssignSpeakers([]runner.Segment{{Start: 0, End: 3, Text: "one two three", Words: words}}, turns)
	if len(segs) != 2 || segs[0].Text != "one two" || segs[1].Text != "three" || segs[1].Speaker != 1 {
		t.Errorf("word-level: %+v", segs)
	}
	if segs := AssignSpeakers([]runner.Segment{{Start: 0, End: 1, Text: "x"}}, nil); len(segs) != 1 || segs[0].Speaker != -1 {
		t.Errorf("no turns: %+v", segs)
	}
	if SpeakerLabel(0) != "A" || SpeakerLabel(25) != "Z" || SpeakerLabel(26) != "AA" || SpeakerLabel(27) != "AB" {
		t.Error("speaker labels")
	}
}

func TestVoices(t *testing.T) {
	ts := newTestServer(t)
	ref := audio.EncodeWAV(make([]byte, 2*16000*2), 16000) // two seconds
	const path = "/v1/audio/voices"

	wantError(t, postForm(t, ts, path, "audio_sample", ref, "name", "anna"), 400, "consent is required")
	wantError(t, postForm(t, ts, path, "audio_sample", ref, "consent", "ok"), 400, "name must be")
	wantError(t, postForm(t, ts, path, "audio_sample", audio.EncodeWAV(make([]byte, 3200), 16000), "name", "anna", "consent", "ok"), 400, "between 1 and 30 s")
	wantError(t, postForm(t, ts, path, "audio_sample", nil, "name", "anna", "consent", "ok"), 400, "audio_sample is required")

	resp := postForm(t, ts, path, "audio_sample", ref, "name", "Anna", "consent", "Anna agreed", "transcript", "three little words")
	var v struct{ ID, Object, Type, Name, Consent string }
	if raw := body(t, resp); json.Unmarshal(raw, &v) != nil || v.Object != "audio.voice" || v.Type != "audio_sample" || !strings.HasPrefix(v.ID, "voice_") || v.Consent != "Anna agreed" {
		t.Fatalf("create: %s", raw)
	}
	wantError(t, postForm(t, ts, path, "audio_sample", ref, "name", "anna", "consent", "ok"), 400, "already exists")

	// A cloning model speaks with the voice, by name or by id; the reference
	// engine shows it received the reference by its pitch (660 Hz for a
	// three-word transcript).
	pitch := func(voice any) float64 {
		resp := speech(t, ts, map[string]any{"model": "ref-clone", "voice": voice, "input": "x", "response_format": "wav"})
		raw := body(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("speech with %v: %s", voice, raw)
		}
		s, rate, _ := audio.DecodeWAV(raw)
		s = s[:rate/10]
		n := 0
		for i := 1; i < len(s); i++ {
			if (s[i-1] < 0) != (s[i] < 0) {
				n++
			}
		}
		return float64(n) / 2 / 0.1
	}
	for _, voice := range []any{"anna", v.ID, map[string]string{"id": v.ID}} {
		if hz := pitch(voice); hz < 650 || hz > 670 {
			t.Errorf("voice %v: %.0f Hz, want about 660", voice, hz)
		}
	}
	// A model without the capability refuses a custom voice instead of
	// quietly using some other one.
	wantError(t, speech(t, ts, map[string]any{"model": "ref-tts", "voice": "anna", "input": "x"}), 400, "cannot clone voices")
	wantError(t, speech(t, ts, map[string]any{"model": "ref-tts", "voice": map[string]string{"id": v.ID}, "input": "x"}), 400, "cannot clone voices")

	// A custom voice does not hide a built-in voice of the same name from a
	// model that cannot clone: "High" is still the reference engine's own.
	resp = postForm(t, ts, path, "audio_sample", ref, "name", "High", "consent", "ok")
	var high struct{ ID string }
	json.Unmarshal(body(t, resp), &high)
	if resp := speech(t, ts, map[string]any{"model": "ref-tts", "voice": "high", "input": "x", "response_format": "wav"}); resp.StatusCode != 200 {
		t.Errorf("built-in voice hidden by a custom one: %s", body(t, resp))
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+path+"/"+high.ID, nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	resp, _ = http.Get(ts.URL + path)
	var list struct{ Data []struct{ ID string } }
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Data) != 1 || list.Data[0].ID != v.ID {
		t.Errorf("list: %+v", list)
	}
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+path+"/anna", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	wantError(t, speech(t, ts, map[string]any{"model": "ref-clone", "voice": "anna", "input": "x"}), 400, "anna")
}

// ---- a minimal WebSocket client, for the realtime tests ----

type wsClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialWS(t *testing.T, ts *httptest.Server, path string) *wsClient {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", path)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("handshake: %v %+v", err, resp)
	}
	return &wsClient{conn: conn, br: br}
}

func (c *wsClient) send(t *testing.T, v any) {
	t.Helper()
	if err := c.write(v); err != nil {
		t.Fatal(err)
	}
}

// write sends one masked text frame. Unlike send it may be called from any goroutine.
func (c *wsClient) write(v any) error {
	payload, _ := json.Marshal(v)
	frame := []byte{0x81}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 1<<16:
		frame = append(frame, 0x80|126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	var key [4]byte
	rand.Read(key[:])
	frame = append(frame, key[:]...)
	for i, b := range payload {
		frame = append(frame, b^key[i&3])
	}
	_, err := c.conn.Write(frame)
	return err
}

func (c *wsClient) recv(t *testing.T) map[string]any {
	t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		t.Fatalf("read: %v", err)
	}
	n := uint64(head[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		io.ReadFull(c.br, b[:])
		n = uint64(binary.BigEndian.Uint16(b[:]))
	} else if n == 127 {
		var b [8]byte
		io.ReadFull(c.br, b[:])
		n = binary.BigEndian.Uint64(b[:])
	}
	payload := make([]byte, n)
	io.ReadFull(c.br, payload)
	var ev map[string]any
	json.Unmarshal(payload, &ev)
	return ev
}

// until reads events up to and including the first of the wanted type.
func (c *wsClient) until(t *testing.T, want string) (seen []string, ev map[string]any) {
	t.Helper()
	for {
		ev = c.recv(t)
		kind, _ := ev["type"].(string)
		seen = append(seen, kind)
		if kind == want {
			return seen, ev
		}
		if kind == "error" {
			t.Fatalf("error event while waiting for %s: %v", want, ev["error"])
		}
	}
}

func TestRealtimeTranscription(t *testing.T) {
	ts := newTestServer(t)
	c := dialWS(t, ts, "/v1/realtime?model=ref-stt")
	if ev := c.recv(t); ev["type"] != "session.created" || ev["event_id"] == "" {
		t.Fatalf("first event: %v", ev)
	}
	c.send(t, map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription",
		"audio": map[string]any{"input": map[string]any{"turn_detection": map[string]any{"type": "server_vad", "silence_duration_ms": 300}}}}})
	_, ev := c.until(t, "session.updated")
	td := ev["session"].(map[string]any)["audio"].(map[string]any)["input"].(map[string]any)["turn_detection"].(map[string]any)
	if td["silence_duration_ms"].(float64) != 300 {
		t.Errorf("turn detection not applied: %v", td)
	}

	// Two utterances of two words each, separated by a second of silence,
	// streamed in 100 ms pieces like a microphone would.
	utter := body(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "one two", "response_format": "pcm"}))
	stream := append(append(append([]byte{}, utter...), make([]byte, 24000*2)...), utter...)
	stream = append(stream, make([]byte, 24000*2)...)
	// The sender is waited for below: it must not outlive the test, and
	// t.Fatal is not allowed outside the test's own goroutine.
	sent := make(chan error, 1)
	go func() {
		defer close(sent)
		for i := 0; i < len(stream); i += 4800 {
			end := i + 4800
			if end > len(stream) {
				end = len(stream)
			}
			if err := c.write(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(stream[i:end])}); err != nil {
				sent <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// Events of the second utterance may begin before the first has finished
	// transcribing, so check each item's own events rather than a global order.
	perItem := map[string][]string{}
	var items []string
	transcripts := map[string]any{}
	for len(transcripts) < 2 {
		ev := c.recv(t)
		kind, _ := ev["type"].(string)
		if kind == "error" {
			t.Fatalf("error event: %v", ev["error"])
		}
		id, _ := ev["item_id"].(string)
		if item, ok := ev["item"].(map[string]any); ok {
			id, _ = item["id"].(string)
		}
		if id == "" {
			continue
		}
		if perItem[id] == nil {
			items = append(items, id)
		}
		perItem[id] = append(perItem[id], kind)
		if kind == "conversation.item.input_audio_transcription.completed" {
			transcripts[id] = ev["transcript"]
		}
	}
	if err := <-sent; err != nil {
		t.Fatalf("streaming audio: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %v", len(items), perItem)
	}
	const wantOrder = "input_audio_buffer.speech_started input_audio_buffer.speech_stopped input_audio_buffer.committed " +
		"conversation.item.added conversation.item.input_audio_transcription.delta conversation.item.input_audio_transcription.delta " +
		"conversation.item.input_audio_transcription.completed"
	for _, id := range items {
		if got := strings.Join(perItem[id], " "); got != wantOrder {
			t.Errorf("item events:\n got %s\nwant %s", got, wantOrder)
		}
		if transcripts[id] != "word1 word2" {
			t.Errorf("transcript %q, want the two words of one utterance", transcripts[id])
		}
	}

	// With turn detection off, the client decides where an utterance ends.
	c.send(t, map[string]any{"type": "session.update", "session": map[string]any{"type": "transcription",
		"audio": map[string]any{"input": map[string]any{"turn_detection": nil}}}})
	c.until(t, "session.updated")
	c.send(t, map[string]any{"type": "input_audio_buffer.clear"})
	c.until(t, "input_audio_buffer.cleared")
	c.send(t, map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(utter)})
	c.send(t, map[string]any{"type": "input_audio_buffer.commit"})
	if _, done := c.until(t, "conversation.item.input_audio_transcription.completed"); done["transcript"] != "word1 word2" {
		t.Errorf("manual commit: transcript %q", done["transcript"])
	}

	// Things this server does not do are refused with an error event, and the session carries on.
	c.send(t, map[string]any{"type": "response.create"})
	if ev := c.recv(t); ev["type"] != "error" || !strings.Contains(fmt.Sprint(ev["error"]), "transcription sessions") {
		t.Errorf("unsupported event: %v", ev)
	}
	c.send(t, map[string]any{"type": "session.update", "session": map[string]any{"type": "realtime"}})
	if ev := c.recv(t); ev["type"] != "error" || !strings.Contains(fmt.Sprint(ev["error"]), "language model") {
		t.Errorf("conversation session: %v", ev)
	}
}

func TestRealtimeNeedsWebSocket(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/realtime")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("plain GET: %d, want 426", resp.StatusCode)
	}
}

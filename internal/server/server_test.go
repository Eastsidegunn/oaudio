package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eastsidegunn/oaudio/internal/audio"
	"github.com/eastsidegunn/oaudio/internal/catalog"
	"github.com/eastsidegunn/oaudio/internal/engines/ref"
	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/internal/sched"
	"github.com/eastsidegunn/oaudio/internal/store"
	"github.com/eastsidegunn/oaudio/runner"
)

func refManifest(name, task string, caps ...string) string {
	b, _ := json.Marshal(map[string]any{
		"schema": manifest.Schema, "name": name, "tag": "v1", "aliases": []string{"latest"},
		"task": task, "engine": "reference", "upstream": map[string]string{"model": "oaudio/reference"},
		"source": map[string]string{"type": "system"}, "license": map[string]any{"spdx": "Apache-2.0", "commercial_use": true},
		"capabilities": caps, "audio": map[string]int{"sample_rate": 16000},
	})
	return string(b)
}

// newTestServer runs the real HTTP API over the reference engine. Runners are
// real runner.Handler servers reached over HTTP, only not separate processes.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("OAUDIO_FFMPEG", "off") // results must not depend on the machine
	home := t.TempDir()
	st := store.New(home)
	os.MkdirAll(st.ManifestsDir(), 0o755)
	for name, body := range map[string]string{
		"ref-tts":    refManifest("ref-tts", "tts", "speed"),
		"ref-stt":    refManifest("ref-stt", "stt", "segment_timestamps", "word_timestamps"),
		"ref-plain":  refManifest("ref-plain", "stt"),
		"ref-stream": refManifest("ref-stream", "stt", "stream"),
		"ref-vad":    refManifest("ref-vad", "vad"),
		"ref-diar":   refManifest("ref-diar", "diarize"),
		"ref-clone":  refManifest("ref-clone", "tts", "voice_clone"),
	} {
		os.WriteFile(filepath.Join(st.ManifestsDir(), name+".json"), []byte(body), 0o644)
	}
	cat, err := catalog.Load(st.ManifestsDir())
	if err != nil {
		t.Fatal(err)
	}
	launch := func(ctx context.Context, m *manifest.Manifest) (*sched.Loaded, error) {
		engine, _ := ref.New(runner.Model{})
		rs := httptest.NewServer(runner.Handler(engine))
		conn, err := runner.Dial(ctx, strings.TrimPrefix(rs.URL, "http://"))
		if err != nil {
			return nil, err
		}
		return &sched.Loaded{Conn: conn, Stop: rs.Close}, nil
	}
	srv := New(Config{Version: "test", Store: st, Catalog: cat, Launch: launch,
		Aliases: map[string]string{"tts-1": "ref-tts", "whisper-1": "ref-stt"}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return ts
}

func speech(t *testing.T, ts *httptest.Server, body map[string]any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func transcribe(t *testing.T, ts *httptest.Server, path string, wav []byte, fields ...string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i+1 < len(fields); i += 2 {
		mw.WriteField(fields[i], fields[i+1])
	}
	if wav != nil {
		part, _ := mw.CreateFormFile("file", "audio.wav")
		part.Write(wav)
	}
	mw.Close()
	resp, err := http.Post(ts.URL+path, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func wantError(t *testing.T, resp *http.Response, status int, contains string) {
	t.Helper()
	b := body(t, resp)
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if resp.StatusCode != status || json.Unmarshal(b, &e) != nil || !strings.Contains(e.Error.Message, contains) || e.Error.Type == "" {
		t.Errorf("got %d %s; want %d with an OpenAI-style error mentioning %q", resp.StatusCode, b, status, contains)
	}
}

func TestSpeechFormats(t *testing.T) {
	ts := newTestServer(t)

	// Without ffmpeg the default format falls back to wav, at the model's rate.
	resp := speech(t, ts, map[string]any{"model": "ref-tts", "input": "one two three. four five."})
	wav := body(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("status %d type %q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), wav)
	}
	samples, rate, err := audio.DecodeWAV(wav)
	if err != nil || rate != 24000 {
		t.Fatalf("wav: rate %d err %v", rate, err)
	}
	if want := 5 * 24000 * 160 / 1000; len(samples) != want { // five words of 160 ms
		t.Errorf("%d samples, want %d", len(samples), want)
	}

	// pcm is headerless 24 kHz, which is what OpenAI clients assume.
	resp = speech(t, ts, map[string]any{"model": "ref-tts", "input": "one two", "response_format": "pcm", "voice": "high"})
	pcm := body(t, resp)
	if ct := resp.Header.Get("Content-Type"); ct != "audio/pcm;rate=24000" || len(pcm) != 2*2*24000*160/1000 {
		t.Errorf("pcm: type %q, %d bytes", ct, len(pcm))
	}

	// Voice may be an object, as newer OpenAI clients send it.
	resp = speech(t, ts, map[string]any{"model": "ref-tts", "input": "one", "response_format": "wav", "voice": map[string]string{"id": "low"}})
	if resp.StatusCode != 200 {
		t.Errorf("voice object rejected: %s", body(t, resp))
	}

	// speed halves the duration.
	resp = speech(t, ts, map[string]any{"model": "ref-tts", "input": "one two", "response_format": "pcm", "speed": 2.0})
	if fast := body(t, resp); len(fast) != len(pcm)/2 {
		t.Errorf("speed 2: %d bytes, want %d", len(fast), len(pcm)/2)
	}

	// SSE wraps the same audio in speech.audio.delta events.
	resp = speech(t, ts, map[string]any{"model": "ref-tts", "input": "one two", "response_format": "pcm", "voice": "high", "stream_format": "sse"})
	var got []byte
	done := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<22)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == sc.Text() || line == "" {
			continue
		}
		var ev struct{ Type, Audio string }
		json.Unmarshal([]byte(line), &ev)
		switch ev.Type {
		case "speech.audio.delta":
			chunk, _ := base64.StdEncoding.DecodeString(ev.Audio)
			got = append(got, chunk...)
		case "speech.audio.done":
			done = true
		}
	}
	if !done || !bytes.Equal(got, pcm) {
		t.Errorf("sse: done=%v, %d bytes, want %d identical bytes", done, len(got), len(pcm))
	}
}

func TestSpeechErrors(t *testing.T) {
	ts := newTestServer(t)
	wantError(t, speech(t, ts, map[string]any{"model": "nope", "input": "x"}), 404, "not found")
	wantError(t, speech(t, ts, map[string]any{"model": "ref-stt", "input": "x"}), 400, "does not synthesize")
	wantError(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "  "}), 400, "input is required")
	wantError(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "x", "voice": "nobody"}), 400, "nobody")
	wantError(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "x", "response_format": "ogg"}), 400, "not supported")
	wantError(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "x", "response_format": "mp3"}), 400, "ffmpeg")
	wantError(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "x", "speed": 9}), 400, "speed")
	wantError(t, speech(t, ts, map[string]any{"model": "whisper:large-v3-turbo", "input": "x"}), 400, "does not synthesize")
	wantError(t, speech(t, ts, map[string]any{"model": "kokoro", "input": "x"}), 404, "oaudio pull kokoro:v1.0")
}

func TestTranscriptionFormats(t *testing.T) {
	ts := newTestServer(t)
	// Make audio with the tts model, at a rate the stt model must resample from.
	wav := body(t, speech(t, ts, map[string]any{"model": "ref-tts", "input": "alpha beta gamma", "response_format": "wav"}))

	resp := transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt")
	var js struct{ Text string }
	if b := body(t, resp); resp.StatusCode != 200 || json.Unmarshal(b, &js) != nil || js.Text != "word1 word2 word3" {
		t.Fatalf("json: %d %s", resp.StatusCode, b)
	}

	resp = transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt", "response_format", "text")
	if b := string(body(t, resp)); b != "word1 word2 word3\n" {
		t.Errorf("text: %q", b)
	}

	resp = transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt", "response_format", "srt")
	if b := string(body(t, resp)); !strings.HasPrefix(b, "1\n00:00:00,000 --> 00:00:00,100\nword1\n\n2\n00:00:00,160 --> 00:00:00,260\nword2\n") {
		t.Errorf("srt:\n%s", b)
	}

	resp = transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt", "response_format", "vtt")
	if b := string(body(t, resp)); !strings.HasPrefix(b, "WEBVTT\n\n00:00:00.000 --> 00:00:00.100\nword1\n") {
		t.Errorf("vtt:\n%s", b)
	}

	resp = transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt", "response_format", "verbose_json",
		"timestamp_granularities[]", "word", "timestamp_granularities[]", "segment", "language", "en")
	var v struct {
		Task, Language, Text string
		Duration             float64
		Segments             []struct {
			ID         int
			Start, End float64
			Text       string
			Tokens     []int
		}
		Words []struct {
			Word       string
			Start, End float64
		}
	}
	if b := body(t, resp); json.Unmarshal(b, &v) != nil || v.Task != "transcribe" || v.Language != "english" ||
		len(v.Segments) != 3 || len(v.Words) != 3 || v.Segments[2].ID != 2 || v.Segments[0].Tokens == nil ||
		v.Words[1].Word != "word2" || v.Duration < 0.47 || v.Duration > 0.49 {
		t.Errorf("verbose_json: %s", b)
	}

	// Streaming: one delta per segment, then the full text.
	resp = transcribe(t, ts, "/v1/audio/transcriptions", wav, "model", "ref-stt", "stream", "true")
	var deltas []string
	final := ""
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev struct{ Type, Delta, Text string }
		if json.Unmarshal([]byte(strings.TrimPrefix(sc.Text(), "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "transcript.text.delta" {
			deltas = append(deltas, ev.Delta)
		} else if ev.Type == "transcript.text.done" {
			final = ev.Text
		}
	}
	if strings.Join(deltas, "") != "word1 word2 word3" || len(deltas) != 3 || final != "word1 word2 word3" {
		t.Errorf("stream: deltas %q final %q", deltas, final)
	}
}

func TestTranscriptionErrors(t *testing.T) {
	ts := newTestServer(t)
	wav := audio.EncodeWAV(make([]byte, 3200), 16000)
	const p = "/v1/audio/transcriptions"
	wantError(t, transcribe(t, ts, p, nil, "model", "ref-stt"), 400, "file is required")
	wantError(t, transcribe(t, ts, p, wav, "model", "ref-tts"), 400, "does not transcribe")
	wantError(t, transcribe(t, ts, p, wav), 400, "model is required")
	wantError(t, transcribe(t, ts, p, wav, "model", "ref-stt", "response_format", "xml"), 400, "not supported")
	wantError(t, transcribe(t, ts, p, []byte("not audio"), "model", "ref-stt"), 400, "ffmpeg")
	// Capabilities a model does not declare are refused, not silently ignored.
	wantError(t, transcribe(t, ts, p, wav, "model", "ref-plain", "response_format", "srt"), 400, "does not produce timestamps")
	wantError(t, transcribe(t, ts, p, wav, "model", "ref-stt", "prompt", "hint"), 400, "does not accept a prompt")
	wantError(t, transcribe(t, ts, p, wav, "model", "ref-stt", "timestamp_granularities[]", "word"), 400, "verbose_json")
	wantError(t, transcribe(t, ts, "/v1/audio/translations", wav, "model", "ref-stt"), 400, "does not translate")
}

func TestModelsAndLifecycle(t *testing.T) {
	ts := newTestServer(t)
	get := func(path string, v any) int {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(v)
		return resp.StatusCode
	}
	var list struct {
		Object string
		Data   []struct{ ID, Object string }
	}
	get("/v1/models", &list)
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = m.Object == "model"
	}
	if list.Object != "list" || !ids["ref-tts:v1"] || !ids["ref-stt:v1"] || ids["kokoro:v1.0"] {
		t.Errorf("/v1/models lists %v; want runnable models only", ids)
	}
	var one struct{ ID string }
	if get("/v1/models/ref-tts", &one) != 200 || one.ID != "ref-tts:v1" {
		t.Errorf("/v1/models/ref-tts -> %+v", one)
	}
	if get("/v1/models/kokoro", &one) != 404 {
		t.Error("a model that is not installed was reported as available")
	}

	// Aliases let clients with hard-coded OpenAI model names work unchanged.
	if resp := speech(t, ts, map[string]any{"model": "tts-1", "input": "x", "keep_alive": "10m"}); resp.StatusCode != 200 {
		t.Fatalf("alias: %s", body(t, resp))
	}
	var ps struct {
		Models []struct {
			Model     string
			ExpiresAt time.Time `json:"expires_at"`
		}
	}
	get("/api/ps", &ps)
	if len(ps.Models) != 1 || ps.Models[0].Model != "ref-tts:v1" {
		t.Fatalf("ps: %+v", ps)
	}
	if left := time.Until(ps.Models[0].ExpiresAt); left < 9*time.Minute || left > 10*time.Minute {
		t.Errorf("keep_alive 10m gave an expiry %s away", left)
	}
	resp, _ := http.Post(ts.URL+"/api/stop", "application/json", strings.NewReader(`{"model":"ref-tts"}`))
	resp.Body.Close()
	get("/api/ps", &ps)
	if resp.StatusCode != 200 || len(ps.Models) != 0 {
		t.Errorf("stop: status %d, still loaded: %+v", resp.StatusCode, ps.Models)
	}
	// keep_alive 0 unloads as soon as the request ends.
	speech(t, ts, map[string]any{"model": "ref-tts", "input": "x", "keep_alive": 0}).Body.Close()
	get("/api/ps", &ps)
	if len(ps.Models) != 0 {
		t.Errorf("keep_alive 0 left the model loaded: %+v", ps.Models)
	}
}

func TestSplitSentences(t *testing.T) {
	cases := map[string][]string{
		"Hello there. How are you? Fine!":  {"Hello there.", "How are you?", "Fine!"},
		"Pi is 3.14 exactly.":              {"Pi is 3.14 exactly."},
		"안녕하세요. 오늘 회의는 세 시입니다.":            {"안녕하세요.", "오늘 회의는 세 시입니다."},
		"今日は晴れです。明日は雨です。":                  {"今日は晴れです。", "明日は雨です。"},
		"line one\nline two":               {"line one", "line two"},
		"no terminator":                    {"no terminator"},
		"  \n ":                            nil,
		strings.Repeat("word ", 200) + ".": nil, // checked below
	}
	for in, want := range cases {
		got := SplitSentences(in)
		if strings.HasPrefix(in, "word word") {
			total := 0
			for _, g := range got {
				if n := len([]rune(g)); n > 400 {
					t.Errorf("chunk of %d runes exceeds the limit", n)
				}
				total += len(strings.Fields(g))
			}
			if total != 201 || len(got) < 3 {
				t.Errorf("long input: %d chunks, %d words; want every word kept", len(got), total)
			}
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestJoinText(t *testing.T) {
	text := ""
	for _, seg := range []string{" Hello world.", " Next one.", "今日は", "晴れです。", "Then English"} {
		text = JoinText(text, seg)
	}
	if want := "Hello world. Next one. 今日は晴れです。Then English"; text != want {
		t.Errorf("got %q, want %q", text, want)
	}
}

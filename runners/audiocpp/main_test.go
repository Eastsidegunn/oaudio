//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eastsidegunn/oaudio/runner"
)

func wav(format, bits, channels, rate int, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(format))
	binary.Write(&b, binary.LittleEndian, uint16(channels))
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*channels*bits/8))
	binary.Write(&b, binary.LittleEndian, uint16(channels*bits/8))
	binary.Write(&b, binary.LittleEndian, uint16(bits))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func TestWavPCM16(t *testing.T) {
	pcm, rate, err := wavPCM16(wav(1, 16, 1, 24000, []byte{1, 0, 2, 0}))
	if err != nil || rate != 24000 || !bytes.Equal(pcm, []byte{1, 0, 2, 0}) {
		t.Fatalf("16-bit: %v %d %v", pcm, rate, err)
	}

	var f bytes.Buffer
	binary.Write(&f, binary.LittleEndian, []float32{0.5, -1})
	pcm, rate, err = wavPCM16(wav(3, 32, 1, 44100, f.Bytes()))
	if err != nil || rate != 44100 {
		t.Fatalf("float: %d %v", rate, err)
	}
	got := []int16{int16(binary.LittleEndian.Uint16(pcm)), int16(binary.LittleEndian.Uint16(pcm[2:]))}
	if got[0] != 16384 || got[1] != math.MinInt16 {
		t.Fatalf("float samples: %v", got)
	}

	if _, _, err := wavPCM16(wav(1, 16, 2, 24000, []byte{0, 0, 0, 0})); err == nil {
		t.Fatal("stereo was accepted")
	}
	if _, _, err := wavPCM16([]byte("not audio")); err == nil {
		t.Fatal("garbage was accepted")
	}
}

func TestServerMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":"no such model"}`:              "no such model",
		`{"error":{"message":"bad","type":"x"}}`: "bad",
		"plain text\n":                           "plain text",
	} {
		if got := serverMessage([]byte(body)); got != want {
			t.Errorf("%q: got %q, want %q", body, got, want)
		}
	}
}

// fake stands in for audiocpp_server and records what it was asked.
func fake(t *testing.T, e *engine, reply func(w http.ResponseWriter)) *map[string]any {
	t.Helper()
	asked := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &asked)
		reply(w)
	}))
	t.Cleanup(srv.Close)
	e.base, e.client = srv.URL, srv.Client()
	return &asked
}

func TestSynthesizeStreaming(t *testing.T) {
	e := &engine{rate: 24000, streaming: true, refText: "optional", langs: map[string]bool{"ko": true}}
	asked := fake(t, e, func(w http.ResponseWriter) {
		// Three bytes, then three more: a sample split across two writes.
		w.Write([]byte{1, 2, 3})
		w.(http.Flusher).Flush()
		w.Write([]byte{4, 5, 6})
	})
	var got []byte
	err := e.Synthesize(context.Background(), runner.SynthesisRequest{
		Text: "안녕하세요", Language: "ko-KR",
		Reference: &runner.Reference{AudioPath: "/voices/a.wav", Text: "참조 문장"},
	}, func(pcm []byte) error {
		if len(pcm)%2 != 0 {
			t.Errorf("emitted half a sample: %d bytes", len(pcm))
		}
		got = append(got, pcm...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("audio: %v", got)
	}
	a := *asked
	if a["language"] != "ko" || a["voice_ref"] != "/voices/a.wav" || a["reference_text"] != "참조 문장" ||
		a["response_format"] != "pcm" || a["stream_format"] != "audio" {
		t.Fatalf("request: %v", a)
	}
}

func TestSynthesizeWithoutTranscript(t *testing.T) {
	e := &engine{rate: 24000, streaming: true, refText: "optional", noTextOptions: map[string]any{"x_vector_only_mode": true}}
	asked := fake(t, e, func(w http.ResponseWriter) {})
	err := e.Synthesize(context.Background(), runner.SynthesisRequest{Text: "a", Reference: &runner.Reference{AudioPath: "/a.wav"}},
		func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	a := *asked
	options, _ := a["options"].(map[string]any)
	if _, has := a["reference_text"]; has || options["x_vector_only_mode"] != true {
		t.Fatalf("request: %v", a)
	}
}

func TestSynthesizeRefusals(t *testing.T) {
	e := &engine{rate: 24000, streaming: true, refText: "required", langs: map[string]bool{"ko": true}}
	fake(t, e, func(w http.ResponseWriter) { t.Error("the server should not have been called") })
	none := func([]byte) error { return nil }
	for name, req := range map[string]runner.SynthesisRequest{
		"no voice":       {Text: "a"},
		"no transcript":  {Text: "a", Reference: &runner.Reference{AudioPath: "/a.wav"}},
		"wrong language": {Text: "a", Language: "fr", Reference: &runner.Reference{AudioPath: "/a.wav", Text: "b"}},
	} {
		if err := e.Synthesize(context.Background(), req, none); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSynthesizeServerError(t *testing.T) {
	e := &engine{rate: 24000, noRefOK: true}
	fake(t, e, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"text is empty"}}`))
	})
	err := e.Synthesize(context.Background(), runner.SynthesisRequest{Text: "a"}, func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "text is empty") {
		t.Fatalf("error: %v", err)
	}
}

package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eastsidegunn/oaudio/runner"
)

// The test binary doubles as a fake speechcpp, selected by environment
// variables, so the runner's side of the worker protocol is tested without
// the engine. FAKE_SPEECHCPP picks the behaviour.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_SPEECHCPP"); mode != "" {
		fakeSpeechcpp(mode, os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeSpeechcpp(mode string, args []string) {
	out := json.NewEncoder(os.Stdout)
	if len(args) > 0 && args[0] == "vad" {
		// Two regions, whatever the audio.
		fmt.Println(`{"file":"x.wav","regions":[{"start":1.0,"end":3.0},{"start":5.0,"end":8.5}]}`)
		return
	}
	switch mode {
	case "fatal":
		out.Encode(map[string]any{"type": "fatal", "error": map[string]any{"code": "model_file", "option": nil, "message": "not a model"}})
		os.Exit(1)
	case "old":
		out.Encode(map[string]any{"type": "ready", "protocol": 3, "version": "0.9.0"})
	default:
		out.Encode(map[string]any{"type": "ready", "protocol": 4, "version": "0.10.1"})
	}
	samples := map[string]int{}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var req map[string]any
		json.Unmarshal(sc.Bytes(), &req)
		id, _ := req["id"].(string)
		switch req["type"] {
		case "chunk":
			pcm, _ := base64.StdEncoding.DecodeString(req["pcm"].(string))
			samples[id] += len(pcm) / 2
		case "transcribe":
			switch {
			case req["language"] == "en":
				out.Encode(map[string]any{"type": "error", "id": id, "error": map[string]any{"code": "out_of_range", "option": "language", "message": "the model does not take en"}})
			case mode == "slow":
				// Wait for the cancel, as a long recognition would.
				for sc.Scan() {
					if strings.Contains(sc.Text(), `"cancel"`) {
						out.Encode(map[string]any{"type": "cancelled", "id": id})
						break
					}
				}
			default:
				// One segment covering the audio, its text the number of
				// samples, so the test can check what was sent.
				n := samples[id]
				out.Encode(map[string]any{"type": "progress", "id": id, "done": 0.5})
				out.Encode(map[string]any{"type": "end", "id": id, "stop": "complete", "text": fmt.Sprint(n),
					"segments": []map[string]any{{"start": 0.0, "end": float64(n) / 16000, "text": fmt.Sprint(n)}}})
			}
			delete(samples, id)
		}
	}
}

func fakeModel(t *testing.T, mode string) runner.Model {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAUDIO_SPEECHCPP", self)
	t.Setenv("FAKE_SPEECHCPP", mode)
	m := runner.Model{Task: "stt", Dir: t.TempDir()}
	m.Audio.SampleRate = 16000
	m.EngineConfig = map[string]string{"model": "m.gguf", "languages": "ja", "vad": "vad.gguf", "whole_seconds": "30"}
	return m
}

func transcribe(t *testing.T, e runner.Engine, ctx context.Context, seconds float64, lang string) ([]runner.Segment, string, error) {
	t.Helper()
	var segs []runner.Segment
	samples := make([]float32, int(seconds*16000))
	l, err := e.(runner.Transcriber).Transcribe(ctx, samples, runner.TranscribeOptions{SampleRate: 16000, Language: lang},
		func(s runner.Segment) error { segs = append(segs, s); return nil })
	return segs, l, err
}

func TestShortRecordingIsOneRequest(t *testing.T) {
	e, err := load(fakeModel(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	segs, lang, err := transcribe(t, e, context.Background(), 2.5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || segs[0].Text != "40000" || segs[0].Start != 0 || segs[0].End != 2.5 {
		t.Errorf("segments %+v, want one of 40000 samples from 0 to 2.5 s", segs)
	}
	if lang != "ja" {
		t.Errorf("language %q, want the model's only one", lang)
	}
	// A second request on the same worker.
	if segs, _, err := transcribe(t, e, context.Background(), 1, "ja"); err != nil || len(segs) != 1 || segs[0].Text != "16000" {
		t.Errorf("second request: %+v %v", segs, err)
	}
}

func TestLongRecordingIsRecognizedByRegion(t *testing.T) {
	e, err := load(fakeModel(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	segs, _, err := transcribe(t, e, context.Background(), 40, "")
	if err != nil {
		t.Fatal(err)
	}
	// The fake detector finds 1-3 s and 5-8.5 s: 2 s and 3.5 s of samples,
	// with times shifted back to the whole recording.
	want := []runner.Segment{{Start: 1, End: 3, Text: "32000"}, {Start: 5, End: 8.5, Text: "56000"}}
	if fmt.Sprint(segs) != fmt.Sprint(want) {
		t.Errorf("segments %+v, want %+v", segs, want)
	}
}

func TestRequestErrorsAreBadRequests(t *testing.T) {
	e, err := load(fakeModel(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	var bad *runner.BadRequestError
	if _, _, err := transcribe(t, e, context.Background(), 1, "ko"); !errors.As(err, &bad) {
		t.Errorf("a language the manifest lacks: %v, want a bad request", err)
	}
	// Refused by the worker itself.
	e.(*engine).langs = nil
	if _, _, err := transcribe(t, e, context.Background(), 1, "en"); !errors.As(err, &bad) || !strings.Contains(err.Error(), "en") {
		t.Errorf("a language the worker refuses: %v, want a bad request", err)
	}
	if _, err := e.(runner.Transcriber).Transcribe(context.Background(), make([]float32, 10),
		runner.TranscribeOptions{SampleRate: 16000, Task: "translate"}, func(runner.Segment) error { return nil }); !errors.As(err, &bad) {
		t.Errorf("translate: %v, want a bad request", err)
	}
	// The worker is still usable afterwards.
	if segs, _, err := transcribe(t, e, context.Background(), 1, ""); err != nil || len(segs) != 1 {
		t.Errorf("after errors: %+v %v", segs, err)
	}
}

func TestCancelReachesTheWorker(t *testing.T) {
	e, err := load(fakeModel(t, "slow"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := transcribe(t, e, ctx, 1, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("err %v, want cancelled", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("cancel took too long")
	}
}

func TestLoadFailures(t *testing.T) {
	if _, err := load(fakeModel(t, "fatal")); err == nil || !strings.Contains(err.Error(), "not a model") {
		t.Errorf("fatal: %v", err)
	}
	if _, err := load(fakeModel(t, "old")); err == nil || !strings.Contains(err.Error(), "protocol 3") {
		t.Errorf("old protocol: %v", err)
	}
	m := fakeModel(t, "ok")
	m.Task = "tts"
	if _, err := load(m); err == nil {
		t.Error("a synthesis model was accepted")
	}
}

func TestCloseStopsTheWorker(t *testing.T) {
	e, err := load(fakeModel(t, "ok"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-e.(*engine).dead:
	default:
		t.Error("worker still running after Close")
	}
}

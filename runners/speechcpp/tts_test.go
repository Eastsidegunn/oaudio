package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eastsidegunn/oaudio/runner"
)

// fakeSynthesizer is a synthesis worker. Each request's audio is its voice
// name, then its speed, as bytes in three chunks, so a test can read back
// what the worker was asked for. "tts-24k" reports a rate the manifest
// does not have.
func fakeSynthesizer(mode string) {
	out := json.NewEncoder(os.Stdout)
	rate := 48000
	if mode == "tts-24k" {
		rate = 24000
	}
	out.Encode(map[string]any{"type": "ready", "protocol": 4, "version": "0.10.1", "model": map[string]any{
		"task": "synthesis", "sample_rate": rate, "voice_recordings": true,
		"voices": []map[string]any{{"name": "none"}, {"name": "sohee"}}}})
	added := map[string]bool{}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req map[string]any
		json.Unmarshal(sc.Bytes(), &req)
		id, _ := req["id"].(string)
		switch req["type"] {
		case "add_voice":
			if _, err := os.Stat(req["path"].(string)); err != nil {
				out.Encode(map[string]any{"type": "error", "id": id, "error": map[string]any{"code": "file", "option": "path", "message": "no such file"}})
				continue
			}
			added[req["name"].(string)] = true
			// The count of voices added so far, for the test of reuse.
			os.WriteFile(os.Getenv("FAKE_ADDED"), []byte(strings.Repeat("x", len(added))), 0o644)
			out.Encode(map[string]any{"type": "end", "id": id})
		case "synthesize":
			voice, _ := req["voice"].(string)
			if voice != "none" && voice != "sohee" && !added[voice] {
				out.Encode(map[string]any{"type": "error", "id": id, "error": map[string]any{"code": "invalid_argument", "option": "voice", "message": "unknown voice " + voice}})
				continue
			}
			if req["text"] == "slow" {
				for sc.Scan() {
					if strings.Contains(sc.Text(), `"cancel"`) {
						out.Encode(map[string]any{"type": "cancelled", "id": id})
						break
					}
				}
				continue
			}
			speed, _ := json.Marshal(req["speed"])
			audio := voice + "|" + string(speed)
			if len(audio)%2 == 1 {
				audio += " "
			}
			out.Encode(map[string]any{"type": "progress", "id": id, "done": 0.5})
			for seq, part := range []string{audio[:2], audio[2:], ""} {
				out.Encode(map[string]any{"type": "chunk", "id": id, "seq": seq, "pcm": base64.StdEncoding.EncodeToString([]byte(part))})
			}
			out.Encode(map[string]any{"type": "end", "id": id, "seed": 1, "samples": len(audio) / 2, "stop": "complete"})
		}
	}
}

func fakeSynthModel(t *testing.T, mode string) runner.Model {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAUDIO_SPEECHCPP", self)
	t.Setenv("FAKE_SPEECHCPP", mode)
	t.Setenv("FAKE_ADDED", filepath.Join(t.TempDir(), "added"))
	m := runner.Model{Task: "tts", Dir: t.TempDir(), DefaultVoice: "none",
		Voices: []runner.Voice{{ID: "none"}, {ID: "sohee", Language: "ko"}}}
	m.Audio.SampleRate = 48000
	m.EngineConfig = map[string]string{"model": "m.gguf", "languages": "ja,ko", "speed": "true"}
	return m
}

func speak(t *testing.T, e runner.Engine, ctx context.Context, req runner.SynthesisRequest) (string, error) {
	t.Helper()
	var audio strings.Builder
	err := e.(runner.Synthesizer).Synthesize(ctx, req, func(pcm []byte) error { audio.Write(pcm); return nil })
	return strings.TrimSpace(audio.String()), err
}

func TestSynthesisVoices(t *testing.T) {
	e, err := load(fakeSynthModel(t, "tts"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if info := e.Info(); info.SampleRate != 48000 || len(info.Tasks) != 1 || info.Tasks[0] != "tts" {
		t.Errorf("info %+v", info)
	}
	for _, c := range []struct{ voice, want string }{{"", "none|null"}, {"SoHee", "sohee|null"}} {
		got, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Voice: c.voice})
		if err != nil || got != c.want {
			t.Errorf("voice %q: %q %v, want %q", c.voice, got, err, c.want)
		}
	}
	got, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Speed: 1.5})
	if err != nil || got != "none|1.5" {
		t.Errorf("speed: %q %v", got, err)
	}
	var bad *runner.BadRequestError
	if _, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Voice: "alloy"}); !errors.As(err, &bad) || !strings.Contains(err.Error(), "sohee") {
		t.Errorf("unknown voice: %v, want a bad request naming the voices", err)
	}
	if _, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Language: "en-US"}); !errors.As(err, &bad) {
		t.Errorf("unspoken language: %v, want a bad request", err)
	}
	if _, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Language: "ko-KR"}); err != nil {
		t.Errorf("a region of a spoken language: %v", err)
	}
}

func TestSynthesisReferenceIsAddedOnce(t *testing.T) {
	e, err := load(fakeSynthModel(t, "tts"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")
	os.WriteFile(a, []byte("a"), 0o644)
	os.WriteFile(b, []byte("b"), 0o644)
	for i, path := range []string{a, a, b, a} {
		got, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Reference: &runner.Reference{AudioPath: path}})
		if err != nil || !strings.HasPrefix(got, "oaudio-") {
			t.Fatalf("request %d: %q %v", i, got, err)
		}
	}
	if n, _ := os.ReadFile(os.Getenv("FAKE_ADDED")); len(n) != 2 {
		t.Errorf("%d voices added for two recordings", len(n))
	}
	// A missing recording is an error, and the worker carries on.
	if _, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Reference: &runner.Reference{AudioPath: filepath.Join(dir, "gone.wav")}}); err == nil {
		t.Error("missing reference accepted")
	}
	if _, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x"}); err != nil {
		t.Errorf("after an error: %v", err)
	}
}

func TestSynthesisRefusals(t *testing.T) {
	m := fakeSynthModel(t, "tts")
	delete(m.EngineConfig, "speed")
	e, err := load(m)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	var bad *runner.BadRequestError
	if _, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Speed: 2}); !errors.As(err, &bad) {
		t.Errorf("speed without the option: %v, want a bad request", err)
	}
	e.(*engine).synth.recordings = false
	if _, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x", Reference: &runner.Reference{AudioPath: "r.wav"}}); !errors.As(err, &bad) {
		t.Errorf("reference to a model that cannot clone: %v, want a bad request", err)
	}
	if _, err := e.(runner.Transcriber).Transcribe(context.Background(), make([]float32, 10), runner.TranscribeOptions{SampleRate: 16000},
		func(runner.Segment) error { return nil }); err == nil {
		t.Error("a synthesis model transcribed")
	}
}

func TestSynthesisCancel(t *testing.T) {
	e, err := load(fakeSynthModel(t, "tts"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := speak(t, e, ctx, runner.SynthesisRequest{Text: "slow"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err %v, want cancelled", err)
	}
	if got, err := speak(t, e, context.Background(), runner.SynthesisRequest{Text: "x"}); err != nil || got != "none|null" {
		t.Errorf("after a cancel: %q %v", got, err)
	}
}

func TestSynthesisManifestMustMatchModel(t *testing.T) {
	if _, err := load(fakeSynthModel(t, "tts-24k")); err == nil || !strings.Contains(err.Error(), "24000 Hz") {
		t.Errorf("a wrong sample rate: %v", err)
	}
	m := fakeSynthModel(t, "tts")
	m.Voices = append(m.Voices, runner.Voice{ID: "vivian"})
	if _, err := load(m); err == nil || !strings.Contains(err.Error(), "vivian") {
		t.Errorf("a voice the model lacks: %v", err)
	}
}

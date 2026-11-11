//go:build unix

package runner_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/eastsidegunn/oaudio/internal/engines/ref"
	"github.com/eastsidegunn/oaudio/runner"
)

// The test binary doubles as a runner executable: when re-executed with this
// variable set it serves the reference engine, exactly like a real runner.
func TestMain(m *testing.M) {
	switch os.Getenv("OAUDIO_TEST_RUNNER") {
	case "ref":
		runner.Main(nil, ref.New)
		return
	case "broken":
		os.Stderr.WriteString("cannot load model\n")
		os.Exit(1)
	case "silent":
		time.Sleep(time.Minute)
		return
	}
	os.Exit(m.Run())
}

func launch(t *testing.T, mode string, timeout time.Duration) (*runner.Process, *bytes.Buffer, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAUDIO_TEST_RUNNER", mode)
	logs := new(bytes.Buffer)
	p, err := runner.Launch(context.Background(), exe, nil, logs, timeout)
	if p != nil {
		t.Cleanup(p.Stop)
	}
	return p, logs, err
}

func TestProcessRoundTrip(t *testing.T) {
	p, _, err := launch(t, "ref", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if p.Info.Engine != "reference" || p.Info.SampleRate != 24000 || p.Info.Protocol != runner.ProtocolVersion {
		t.Fatalf("info: %+v", p.Info)
	}
	voices, err := p.Voices(context.Background())
	if err != nil || len(voices) != 2 {
		t.Fatalf("voices: %v %v", voices, err)
	}

	// Text -> PCM over the wire.
	stream, rate, err := p.Synthesize(context.Background(), runner.SynthesisRequest{Text: "one two three four"})
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := io.ReadAll(stream)
	stream.Close()
	if err != nil || rate != 24000 || len(pcm) == 0 {
		t.Fatalf("synthesize: %d bytes at %d Hz, err=%v", len(pcm), rate, err)
	}

	// PCM -> segments over the wire: four words in, four segments out, in order,
	// with increasing timestamps.
	var segs []runner.Segment
	lang, err := p.Transcribe(context.Background(), pcm, runner.TranscribeOptions{SampleRate: rate, WordTimestamps: true}, func(s runner.Segment) error {
		segs = append(segs, s)
		return nil
	})
	if err != nil || lang != "en" {
		t.Fatalf("transcribe: lang=%q err=%v", lang, err)
	}
	if len(segs) != 4 {
		t.Fatalf("got %d segments, want 4: %+v", len(segs), segs)
	}
	for i, s := range segs {
		if s.End <= s.Start || (i > 0 && s.Start < segs[i-1].End) || len(s.Words) != 1 {
			t.Errorf("segment %d has bad timing or words: %+v", i, s)
		}
	}

	// An engine's "bad request" keeps its meaning across the process boundary.
	_, _, err = p.Synthesize(context.Background(), runner.SynthesisRequest{Text: "x", Voice: "nobody"})
	if re, ok := err.(*runner.Error); !ok || re.Status != 400 || !strings.Contains(re.Message, "nobody") {
		t.Errorf("unknown voice: %#v", err)
	}
}

// Stop unloads the model by ending the process.
func TestStopEndsProcess(t *testing.T) {
	p, _, err := launch(t, "ref", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pid := p.Pid()
	p.Stop()
	select {
	case <-p.Exited():
	default:
		t.Fatal("Stop returned before the process exited")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("process %d is still alive after Stop", pid)
	}
}

func TestLaunchFailures(t *testing.T) {
	_, logs, err := launch(t, "broken", 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "before it was ready") {
		t.Errorf("broken runner: %v", err)
	}
	if !strings.Contains(logs.String(), "cannot load model") {
		t.Errorf("the runner's own error was not captured: %q", logs.String())
	}
	start := time.Now()
	_, _, err = launch(t, "silent", 200*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("silent runner: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("a hung runner was not killed promptly")
	}
}

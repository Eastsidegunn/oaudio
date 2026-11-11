// Command runner-check checks a runner executable against the runner
// protocol (docs/runner-protocol.md), the way the daemon would use it.
//
//	go run ./tools/runner-check --manifest model.json --model-dir DIR [--audio speech.wav] -- RUNNER [ARGS...]
//
// It starts the runner, waits for its handshake, reads /v1/info, exercises
// the endpoints of the task the manifest names, checks the shape of every
// answer, and finally closes the runner's stdin and checks that it exits.
// It does not judge recognition or synthesis quality. Exit status 1 means at
// least one check failed.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/eastsidegunn/oaudio/runner"
)

var failed bool

func check(name string, ok bool, detail string, args ...any) {
	report(name, ok, true, detail, args...)
}

// advise reports a SHOULD of the protocol: a miss is a warning, not a failure.
func advise(name string, ok bool, detail string, args ...any) {
	report(name, ok, false, detail, args...)
}

func report(name string, ok, must bool, detail string, args ...any) {
	status := "PASS"
	switch {
	case !ok && must:
		status, failed = "FAIL", true
	case !ok:
		status = "WARN"
	}
	fmt.Printf("%s  %s", status, name)
	if detail != "" {
		fmt.Printf(": "+detail, args...)
	}
	fmt.Println()
}

func main() {
	manifestPath := flag.String("manifest", "", "the model's manifest (JSON)")
	modelDir := flag.String("model-dir", "", "the directory holding the model's files")
	audioPath := flag.String("audio", "", "for stt, vad and diarize: a 16-bit mono WAV file at the manifest's sample rate (default: one second of a tone)")
	text := flag.String("text", "Hello there.", "for tts: the text to speak")
	flag.Parse()
	if *manifestPath == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: runner-check --manifest FILE --model-dir DIR [--audio WAV] -- RUNNER [ARGS...]")
		os.Exit(2)
	}
	var m runner.Model
	var caps struct {
		Capabilities []string `json:"capabilities"`
	}
	data, err := os.ReadFile(*manifestPath)
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err == nil {
		err = json.Unmarshal(data, &caps)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		os.Exit(2)
	}

	args := append(flag.Args()[1:], "--manifest", *manifestPath, "--model-dir", *modelDir)
	start := time.Now()
	p, err := runner.Launch(context.Background(), flag.Arg(0), args, os.Stderr, 5*time.Minute)
	check("handshake within 5 minutes, then /v1/info with protocol 1", err == nil, "%v", errOr(err, time.Since(start).Round(time.Millisecond)))
	if err != nil {
		os.Exit(1)
	}
	base := "http://" + p.Addr
	info := p.Info
	check("info names the engine", info.Engine != "", "%q %q", info.Engine, info.EngineVersion)
	check("info lists the manifest's task", contains(info.Tasks, m.Task), "tasks %v, manifest %q", info.Tasks, m.Task)

	switch m.Task {
	case "tts":
		checkSynthesis(base, p, m, info, *text, contains(caps.Capabilities, "voice_clone"))
	case "stt", "vad", "diarize":
		pcm, rate := loadAudio(*audioPath, m.Audio.SampleRate)
		switch m.Task {
		case "stt":
			checkRecognition(base, p, pcm, rate)
		case "vad":
			checkDetection(base, p, pcm, rate)
		case "diarize":
			checkDiarization(base, p, pcm, rate)
		}
	}
	for _, other := range []struct{ task, method, path string }{
		{"tts", "POST", "/v1/synthesize"}, {"stt", "POST", "/v1/transcribe?sample_rate=16000"},
		{"vad", "POST", "/v1/vad?sample_rate=16000"}, {"diarize", "POST", "/v1/diarize?sample_rate=16000"},
	} {
		if contains(info.Tasks, other.task) {
			continue
		}
		status, _ := do(other.method, base+other.path, []byte("{}"))
		check("a task the runner lacks is refused: "+other.path, status >= 400 && status < 600, "status %d", status)
	}

	stopped := time.Now()
	p.Stop() // closes stdin; kills the runner if it is still there after 3 s
	took := time.Since(stopped)
	check("exits when its stdin closes", took < 3*time.Second, "took %s", took.Round(time.Millisecond))
	if failed {
		os.Exit(1)
	}
}

func checkSynthesis(base string, p *runner.Process, m runner.Model, info runner.Info, text string, clones bool) {
	check("info gives the synthesis sample rate", info.SampleRate > 0, "%d", info.SampleRate)
	status, body := do("GET", base+"/v1/voices", nil)
	var voices []runner.Voice
	check("/v1/voices answers a JSON array", status == 200 && json.Unmarshal(body, &voices) == nil, "status %d, %d voices", status, len(voices))
	for _, v := range voices {
		if v.ID == "" {
			check("every voice has an id", false, "%+v", v)
			break
		}
	}
	req := map[string]any{"text": text}
	if m.DefaultVoice != "" {
		req["voice"] = m.DefaultVoice
	}
	b, _ := json.Marshal(req)
	resp, err := http.Post(base+"/v1/synthesize", "application/json", bytes.NewReader(b))
	if err != nil {
		check("/v1/synthesize", false, "%v", err)
		return
	}
	pcm, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 400 && strings.Contains(string(pcm), "voice") && len(voices) == 0 {
		check("/v1/synthesize without a voice is refused with 400 by a cloning-only model", true, "%s", strings.TrimSpace(string(pcm)))
	} else {
		check("/v1/synthesize answers 200 with X-Sample-Rate equal to info", resp.StatusCode == 200 && resp.Header.Get("X-Sample-Rate") == fmt.Sprint(info.SampleRate),
			"status %d, X-Sample-Rate %q", resp.StatusCode, resp.Header.Get("X-Sample-Rate"))
		check("the body is whole 16-bit samples, some audio", len(pcm) > 0 && len(pcm)%2 == 0, "%d bytes, %.2f s", len(pcm), float64(len(pcm))/2/float64(max(info.SampleRate, 1)))
	}
	if len(voices) > 0 || m.DefaultVoice != "" {
		b, _ = json.Marshal(map[string]any{"text": text, "voice": "no-such-voice-for-runner-check"})
		status, body = do("POST", base+"/v1/synthesize", b)
		check("an unknown voice is refused with 400 and {\"error\"}", status == 400 && hasError(body), "status %d %s", status, short(body))
	}
	// The daemon sends a reference only to a model whose manifest declares
	// voice_clone; any other must refuse one rather than ignore it.
	if !clones {
		b, _ = json.Marshal(map[string]any{"text": text, "reference": map[string]any{"audio_path": "/nonexistent/reference.wav"}})
		status, body = do("POST", base+"/v1/synthesize", b)
		advise("a model that does not clone refuses a reference with 400", status == 400 && hasError(body), "status %d %s", status, short(body))
	}
}

func checkRecognition(base string, p *runner.Process, pcm []byte, rate int) {
	dur := float64(len(pcm)) / 2 / float64(rate)
	resp, err := http.Post(fmt.Sprintf("%s/v1/transcribe?sample_rate=%d", base, rate), "audio/pcm", bytes.NewReader(pcm))
	if err != nil {
		check("/v1/transcribe", false, "%v", err)
		return
	}
	defer resp.Body.Close()
	check("/v1/transcribe answers 200 with application/x-ndjson", resp.StatusCode == 200 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson"),
		"status %d, %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	dec := json.NewDecoder(resp.Body)
	var events []runner.Event
	for {
		var ev runner.Event
		if err := dec.Decode(&ev); err != nil {
			break
		}
		events = append(events, ev)
	}
	ok, detail := len(events) > 0, ""
	last := 0.0
	for i, ev := range events {
		final := i == len(events)-1
		switch {
		case ev.Type == runner.EventSegment && !final && ev.Segment != nil:
			s := ev.Segment
			if s.End < s.Start || s.Start < last-0.5 || s.End > dur+0.5 {
				ok, detail = false, fmt.Sprintf("segment %d at %.2f-%.2f s in %.2f s of audio", i, s.Start, s.End, dur)
			}
			last = s.Start
		case final && (ev.Type == runner.EventDone || ev.Type == runner.EventError):
		default:
			ok, detail = false, fmt.Sprintf("event %d of %d is %q", i, len(events), ev.Type)
		}
	}
	if ok && len(events) > 0 {
		detail = fmt.Sprintf("%d segments, then %q", len(events)-1, events[len(events)-1].Type)
	}
	check("segments in order within the audio, then exactly one done or error", ok, "%s", detail)
	if len(events) > 0 && events[len(events)-1].Type == runner.EventDone {
		var texts []string
		for _, ev := range events[:len(events)-1] {
			texts = append(texts, ev.Segment.Text)
		}
		fmt.Printf("      text: %q\n", strings.Join(texts, ""))
	}
	status, body := do("POST", base+"/v1/transcribe", pcm)
	check("no sample_rate is refused with 400 and {\"error\"}", status == 400 && hasError(body), "status %d %s", status, short(body))
}

func checkDetection(base string, p *runner.Process, pcm []byte, rate int) {
	status, body := do("POST", fmt.Sprintf("%s/v1/vad?sample_rate=%d", base, rate), pcm)
	var out struct {
		Spans []runner.Span `json:"spans"`
	}
	ok := status == 200 && json.Unmarshal(body, &out) == nil && out.Spans != nil
	for i, s := range out.Spans {
		if s.End <= s.Start || (i > 0 && s.Start < out.Spans[i-1].End) {
			ok = false
		}
	}
	check("/v1/vad answers spans in order, not overlapping", ok, "status %d, %d spans", status, len(out.Spans))
}

func checkDiarization(base string, p *runner.Process, pcm []byte, rate int) {
	status, body := do("POST", fmt.Sprintf("%s/v1/diarize?sample_rate=%d", base, rate), pcm)
	var out struct {
		Turns []runner.Turn `json:"turns"`
	}
	ok := status == 200 && json.Unmarshal(body, &out) == nil && out.Turns != nil
	next := 0
	for _, t := range out.Turns {
		if t.End < t.Start || t.Speaker > next || t.Speaker < 0 {
			ok = false
		}
		if t.Speaker == next {
			next++
		}
	}
	check("/v1/diarize answers turns, speakers numbered in order of first appearance", ok, "status %d, %d turns, %d speakers", status, len(out.Turns), next)
}

// loadAudio reads a 16-bit mono WAV at rate, or makes a second of tone.
func loadAudio(path string, rate int) ([]byte, int) {
	if path == "" {
		pcm := make([]byte, rate*2)
		for i := 0; i < rate; i++ {
			v := int16(8000 * math.Sin(float64(i)*440*2*math.Pi/float64(rate)))
			binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
		}
		return pcm, rate
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 44 || string(data[0:4]) != "RIFF" {
		fmt.Fprintln(os.Stderr, "audio: not a WAV file:", path, err)
		os.Exit(2)
	}
	channels := binary.LittleEndian.Uint16(data[22:])
	fileRate := int(binary.LittleEndian.Uint32(data[24:]))
	bits := binary.LittleEndian.Uint16(data[34:])
	if channels != 1 || bits != 16 || fileRate != rate {
		fmt.Fprintf(os.Stderr, "audio: need 16-bit mono at %d Hz, got %d-bit, %d channels, %d Hz\n", rate, bits, channels, fileRate)
		os.Exit(2)
	}
	// Find the data chunk.
	for i := 12; i+8 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[i+4:]))
		if string(data[i:i+4]) == "data" {
			return data[i+8 : min(i+8+size, len(data))], rate
		}
		i += 8 + size + size%2
	}
	fmt.Fprintln(os.Stderr, "audio: no data chunk")
	os.Exit(2)
	return nil, 0
}

func do(method, url string, body []byte) (int, []byte) {
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func hasError(body []byte) bool {
	var e struct {
		Error string `json:"error"`
	}
	return json.Unmarshal(body, &e) == nil && e.Error != ""
}

func short(b []byte) string {
	if len(b) > 100 {
		b = b[:100]
	}
	return fmt.Sprintf("%q", bytes.TrimSpace(b))
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func errOr(err error, ok any) any {
	if err != nil {
		return err
	}
	return ok
}

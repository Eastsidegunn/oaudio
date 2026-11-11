// Command oaudio-runner-speech.cpp runs speech recognition and synthesis
// models through speech.cpp (https://github.com/nyosegawa/speech.cpp, MIT).
//
// speech.cpp has a process mode for exactly this, `speech worker`: one model
// per process, JSON Lines on stdin and stdout, and an exit when stdin closes.
// This runner starts one worker for the model it was asked to load and
// translates the Oaudio runner contract into worker requests. It links
// nothing from speech.cpp; all of the modelling is speech.cpp's work.
//
// The worker does not serve detection models, and speech.cpp's FastConformer
// recognizers drop sentences from a long stretch of speech. A recording
// longer than whole_seconds is therefore cut into regions of speech first,
// by `speech vad` with the defaults of speech.cpp's own `speech asr --vad`,
// and each region is recognized as its own request.
//
// Synthesis is in tts.go.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eastsidegunn/oaudio/runner"
)

// engineVersion is the speech.cpp tag the Makefile built, set at link time.
var engineVersion = "unknown"

// workerProtocol is the version of speech.cpp's worker protocol this runner
// was written against. The worker raises it only for changes a caller must
// follow, so a different number is refused rather than guessed at.
const workerProtocol = 4

// The defaults of `speech asr --vad`, so that a long recording is cut where
// speech.cpp itself would cut it.
var vadFlags = []string{
	"--threshold", "0.5", "--speech-pad-ms", "300", "--min-silence-duration-ms", "500",
	"--max-speech-duration-s", "10", "--min-speech-duration-ms", "250",
}

// message is one line from the worker. Only the members this runner reads.
type message struct {
	Type     string     `json:"type"`
	ID       string     `json:"id"`
	Protocol int        `json:"protocol"`
	Version  string     `json:"version"`
	Text     string     `json:"text"`
	Stop     string     `json:"stop"`
	Untimed  string     `json:"untimed"`
	Segments []wSegment `json:"segments"`
	Error    *wError    `json:"error"`

	// Synthesis: a chunk of audio, and the model ready describes.
	Seq   int          `json:"seq"`
	PCM   string       `json:"pcm"`
	Model *workerModel `json:"model"`
}

type wSegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

type wError struct {
	Code    string  `json:"code"`
	Option  *string `json:"option"`
	Message string  `json:"message"`
}

type engine struct {
	bin   string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan message // every line the worker writes, in order
	dead  chan struct{}
	werr  error // why the worker stopped, once dead is closed
	wmu   sync.Mutex

	rate     int
	langs    []string
	vadModel string  // path of the detection model, or "" to always recognize whole
	whole    float64 // seconds: longer recordings are cut into regions
	next     int     // request ids

	synth *synthesis // set for a synthesis model
}

func speechBinary() (string, error) {
	if p := os.Getenv("OAUDIO_SPEECHCPP"); p != "" {
		return p, nil
	}
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), "speechcpp")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("speechcpp"); err == nil {
		return p, nil
	}
	return "", errors.New("speechcpp not found next to the runner or on PATH; build it with `make -C runners/speechcpp`")
}

func load(m runner.Model) (runner.Engine, error) {
	if m.Task != "stt" && m.Task != "tts" {
		return nil, fmt.Errorf("task %q: this runner recognizes and speaks speech only", m.Task)
	}
	cfg := m.EngineConfig
	if cfg["model"] == "" {
		return nil, errors.New("engine_config.model is required")
	}
	bin, err := speechBinary()
	if err != nil {
		return nil, err
	}
	e := &engine{
		bin:   bin,
		rate:  m.Audio.SampleRate,
		whole: 30,
		lines: make(chan message, 64),
		dead:  make(chan struct{}),
	}
	if v := cfg["languages"]; v != "" {
		e.langs = strings.Split(v, ",")
	}
	if v := cfg["vad"]; v != "" {
		e.vadModel = filepath.Join(m.Dir, v)
	}
	if v := cfg["whole_seconds"]; v != "" {
		if e.whole, err = strconv.ParseFloat(v, 64); err != nil {
			return nil, fmt.Errorf("engine_config.whole_seconds: %w", err)
		}
	}

	args := []string{"worker", filepath.Join(m.Dir, cfg["model"])}
	if v := cfg["options"]; v != "" { // worker flags, such as "--decoding greedy"
		args = append(args, strings.Fields(v)...)
	}
	e.cmd = exec.Command(bin, args...)
	e.cmd.Stderr = os.Stderr // the worker's log goes to the daemon's
	if e.stdin, err = e.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := e.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := e.cmd.Start(); err != nil {
		return nil, fmt.Errorf("start speechcpp: %w", err)
	}
	go e.read(stdout)

	// The worker loads and warms up the model, then says it is ready.
	select {
	case msg, ok := <-e.lines:
		switch {
		case !ok:
			e.Close()
			return nil, fmt.Errorf("speechcpp exited while loading: %v", e.werr)
		case msg.Type == "fatal":
			e.Close()
			return nil, fmt.Errorf("speechcpp: %s", errText(msg.Error))
		case msg.Type != "ready":
			e.Close()
			return nil, fmt.Errorf("speechcpp: expected ready, got %q", msg.Type)
		case msg.Protocol != workerProtocol:
			e.Close()
			return nil, fmt.Errorf("speechcpp speaks worker protocol %d, this runner %d", msg.Protocol, workerProtocol)
		}
		if m.Task == "tts" {
			if e.synth, err = newSynthesis(m, msg.Model); err != nil {
				e.Close()
				return nil, err
			}
		}
	case <-e.dead:
		e.Close()
		return nil, fmt.Errorf("speechcpp exited while loading: %v", e.werr)
	}
	return e, nil
}

// read turns the worker's stdout into messages. The worker's documentation
// says a line that is not a JSON object is a defect, so it ends the runner's
// use of the worker rather than being skipped.
func (e *engine) read(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var msg message
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil || msg.Type == "" {
			e.werr = fmt.Errorf("speechcpp wrote a line that is not a message: %.80q", sc.Text())
			break
		}
		e.lines <- msg
	}
	if e.werr == nil {
		e.werr = sc.Err()
	}
	if err := e.cmd.Wait(); e.werr == nil {
		e.werr = err
	}
	if e.werr == nil {
		e.werr = errors.New("speechcpp exited")
	}
	close(e.lines)
	close(e.dead)
}

func (e *engine) send(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	_, err = e.stdin.Write(append(line, '\n'))
	return err
}

func (e *engine) Info() runner.Info {
	if e.synth != nil {
		return runner.Info{Engine: "speech.cpp", EngineVersion: engineVersion, Tasks: []string{"tts"}, SampleRate: e.rate}
	}
	return runner.Info{Engine: "speech.cpp", EngineVersion: engineVersion, Tasks: []string{"stt"}}
}

func (e *engine) Close() error {
	e.stdin.Close()
	select {
	case <-e.dead:
	case <-time.After(5 * time.Second):
		e.cmd.Process.Kill()
		<-e.dead
	}
	return nil
}

func (e *engine) Transcribe(ctx context.Context, samples []float32, opts runner.TranscribeOptions, emit func(runner.Segment) error) (string, error) {
	if e.synth != nil {
		return "", runner.BadRequest("this model speaks; it does not recognize speech")
	}
	if opts.Task == "translate" {
		return "", runner.BadRequest("this model does not translate")
	}
	if opts.SampleRate != e.rate {
		return "", runner.BadRequest("sample_rate %d: this model takes %d Hz", opts.SampleRate, e.rate)
	}
	lang := opts.Language
	if lang != "" && len(e.langs) > 0 && !contains(e.langs, lang) {
		return "", runner.BadRequest("language %q: this model recognizes %s", lang, strings.Join(e.langs, ", "))
	}

	regions := [][2]float64{{0, float64(len(samples)) / float64(e.rate)}}
	if e.vadModel != "" && regions[0][1] > e.whole {
		var err error
		if regions, err = e.speechRegions(ctx, samples); err != nil {
			return "", err
		}
	}
	for _, reg := range regions {
		from := int(math.Round(reg[0] * float64(e.rate)))
		to := min(int(math.Round(reg[1]*float64(e.rate))), len(samples))
		if from >= to {
			continue
		}
		segs, err := e.recognize(ctx, samples[from:to], lang)
		if err != nil {
			return "", err
		}
		for _, s := range segs {
			s.Start += reg[0]
			s.End += reg[0]
			if err := emit(s); err != nil {
				return "", err
			}
		}
	}
	if lang == "" && len(e.langs) == 1 {
		lang = e.langs[0]
	}
	return lang, nil
}

// recognize sends one recognition request and waits for its answer.
func (e *engine) recognize(ctx context.Context, samples []float32, lang string) ([]runner.Segment, error) {
	e.next++
	id := strconv.Itoa(e.next)
	pcm := runner.PCM16(samples)
	chunk := 10 * e.rate * 2 // bytes per chunk line: 10 s of audio
	for seq := 0; seq*chunk < len(pcm) || seq == 0; seq++ {
		part := pcm[seq*chunk : min((seq+1)*chunk, len(pcm))]
		if err := e.send(map[string]any{"type": "chunk", "id": id, "seq": seq, "pcm": base64.StdEncoding.EncodeToString(part)}); err != nil {
			return nil, e.failure(err)
		}
	}
	req := map[string]any{"type": "transcribe", "id": id, "sample_rate": e.rate, "timestamps": true}
	if lang != "" {
		req["language"] = lang
	}
	if err := e.send(req); err != nil {
		return nil, e.failure(err)
	}

	cancelled := false
	for {
		select {
		case <-ctx.Done():
			if !cancelled {
				e.send(map[string]any{"type": "cancel", "id": id})
				cancelled = true
			}
			ctx = context.Background() // keep reading until the worker answers
			continue
		case msg, ok := <-e.lines:
			if !ok {
				return nil, e.failure(nil)
			}
			if msg.ID != id {
				if msg.ID == "" && msg.Type == "error" {
					return nil, fmt.Errorf("speechcpp: %s", errText(msg.Error))
				}
				continue // progress of this request, or nothing of ours
			}
			switch msg.Type {
			case "progress":
				continue
			case "cancelled":
				return nil, context.Canceled
			case "error":
				return nil, workerError(msg.Error)
			case "end":
				return segments(msg, float64(len(samples))/float64(e.rate)), nil
			}
		}
	}
}

// segments turns a recognition's end into protocol segments. A model that
// cannot time its text gives one segment for the whole request.
func segments(msg message, dur float64) []runner.Segment {
	if len(msg.Segments) == 0 {
		if strings.TrimSpace(msg.Text) == "" {
			return nil
		}
		return []runner.Segment{{Start: 0, End: dur, Text: msg.Text}}
	}
	out := make([]runner.Segment, 0, len(msg.Segments))
	for _, s := range msg.Segments {
		out = append(out, runner.Segment{Start: s.Start, End: s.End, Text: s.Text})
	}
	return out
}

func (e *engine) failure(err error) error {
	select {
	case <-e.dead:
		return fmt.Errorf("speechcpp stopped: %v", e.werr)
	default:
		return err
	}
}

// speechRegions runs `speech vad` on the recording and returns the regions
// where someone speaks, in seconds.
func (e *engine) speechRegions(ctx context.Context, samples []float32) ([][2]float64, error) {
	f, err := os.CreateTemp("", "oaudio-speechcpp-*.wav")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(wav16(samples, e.rate)); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()

	args := append([]string{"vad", e.vadModel, "--format", "json"}, vadFlags...)
	cmd := exec.CommandContext(ctx, e.bin, append(args, f.Name())...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("speechcpp vad: %v: %s", err, lastLine(stderr.String()))
	}
	var res struct {
		Regions []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("speechcpp vad: %w", err)
	}
	regions := make([][2]float64, 0, len(res.Regions))
	for _, r := range res.Regions {
		regions = append(regions, [2]float64{r.Start, r.End})
	}
	return regions, nil
}

// workerError maps a worker error to the protocol: errors about the request
// are 400, the rest the engine's failure.
func workerError(w *wError) error {
	if w == nil {
		return errors.New("speechcpp: error without detail")
	}
	switch w.Code {
	case "invalid_argument", "unsupported", "out_of_range":
		return runner.BadRequest("%s", w.Message)
	}
	return fmt.Errorf("speechcpp: %s", errText(w))
}

func errText(w *wError) string {
	if w == nil {
		return "no detail"
	}
	return w.Code + ": " + w.Message
}

func wav16(samples []float32, rate int) []byte {
	pcm := runner.PCM16(samples)
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	return append(h, pcm...)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func main() {
	runner.Main(os.Args[1:], load)
}

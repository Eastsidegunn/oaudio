// Package ref is the reference runner: a tiny, deterministic, dependency-free
// engine used to test the daemon and to show engine authors the smallest
// complete implementation of the runner contract.
//
// It is not a speech model. Synthesis encodes each word as one tone burst;
// transcription counts tone bursts and reports one segment per burst. That is
// enough to exercise streaming, timestamps and a text -> audio -> text round
// trip without any weights.
package ref

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/eastsidegunn/oaudio/runner"
)

const (
	sampleRate = 24000
	burstMS    = 100
	gapMS      = 60
)

// Engine implements both runner.Synthesizer and runner.Transcriber.
type Engine struct {
	// Delay is added per word to imitate a slow model in tests.
	Delay time.Duration
}

// New returns a reference engine. engine_config "delay_ms" slows it down.
func New(m runner.Model) (runner.Engine, error) {
	e := &Engine{}
	if v := m.EngineConfig["delay_ms"]; v != "" {
		var ms int
		fmt.Sscanf(v, "%d", &ms)
		e.Delay = time.Duration(ms) * time.Millisecond
	}
	return e, nil
}

func (e *Engine) Info() runner.Info {
	return runner.Info{Engine: "reference", EngineVersion: "1", Tasks: []string{"stt", "tts", "vad", "diarize"}, SampleRate: sampleRate}
}

func (e *Engine) Close() error { return nil }

var voices = []runner.Voice{
	{ID: "low", Name: "Low tone", Language: "en"},
	{ID: "high", Name: "High tone", Language: "en"},
}

func (e *Engine) Voices(context.Context) ([]runner.Voice, error) { return voices, nil }

func (e *Engine) Synthesize(ctx context.Context, req runner.SynthesisRequest, emit func([]byte) error) error {
	freq := 440.0
	switch {
	case req.Reference != nil:
		// "Cloning": the pitch is taken from the reference transcript's
		// length, so tests can tell that the reference reached the engine.
		freq = 600 + 20*float64(len(strings.Fields(req.Reference.Text)))
	case req.Voice == "" || req.Voice == "low":
	case req.Voice == "high":
		freq = 880
	default:
		return runner.BadRequest("unknown voice %q (available: low, high)", req.Voice)
	}
	speed := req.Speed
	if speed <= 0 {
		speed = 1
	}
	burst := int(float64(sampleRate*burstMS/1000) / speed)
	gap := int(float64(sampleRate*gapMS/1000) / speed)
	for range strings.Fields(req.Text) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Delay > 0 {
			time.Sleep(e.Delay)
		}
		pcm := make([]byte, (burst+gap)*2)
		for i := 0; i < burst; i++ {
			v := int16(0.5 * 32767 * math.Sin(2*math.Pi*freq*float64(i)/sampleRate))
			pcm[i*2], pcm[i*2+1] = byte(v), byte(v>>8)
		}
		if err := emit(pcm); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Transcribe(ctx context.Context, samples []float32, opts runner.TranscribeOptions, emit func(runner.Segment) error) (string, error) {
	frame := opts.SampleRate / 100 // 10 ms
	if frame == 0 {
		return "", runner.BadRequest("sample rate too low")
	}
	var (
		inBurst bool
		start   int
		n       int
	)
	flush := func(endFrame int) error {
		n++
		seg := runner.Segment{
			Start: float64(start) / 100,
			End:   float64(endFrame) / 100,
			Text:  fmt.Sprintf("word%d", n),
		}
		if opts.WordTimestamps {
			seg.Words = []runner.Word{{Word: seg.Text, Start: seg.Start, End: seg.End}}
		}
		if e.Delay > 0 {
			time.Sleep(e.Delay)
		}
		return emit(seg)
	}
	frames := len(samples) / frame
	for f := 0; f < frames; f++ {
		var energy float64
		for _, s := range samples[f*frame : (f+1)*frame] {
			energy += float64(s) * float64(s)
		}
		loud := energy/float64(frame) > 0.001
		switch {
		case loud && !inBurst:
			inBurst, start = true, f
		case !loud && inBurst:
			inBurst = false
			if err := flush(f); err != nil {
				return "", err
			}
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if inBurst {
		if err := flush(frames); err != nil {
			return "", err
		}
	}
	lang := opts.Language
	if lang == "" {
		lang = "en"
	}
	return lang, nil
}

// bursts returns the tone bursts in samples as (start, end) frame indices of
// 10 ms, with the zero-crossing rate of each, which gives away its pitch.
func bursts(samples []float32, rate int) (spans [][2]int, crossings []float64) {
	frame := rate / 100
	if frame == 0 {
		return nil, nil
	}
	frames := len(samples) / frame
	start := -1
	closeBurst := func(end int) {
		seg := samples[start*frame : end*frame]
		n := 0
		for i := 1; i < len(seg); i++ {
			if (seg[i-1] < 0) != (seg[i] < 0) {
				n++
			}
		}
		spans = append(spans, [2]int{start, end})
		crossings = append(crossings, float64(n)/2/(float64(len(seg))/float64(rate)))
		start = -1
	}
	for f := 0; f < frames; f++ {
		var energy float64
		for _, s := range samples[f*frame : (f+1)*frame] {
			energy += float64(s) * float64(s)
		}
		loud := energy/float64(frame) > 0.001
		if loud && start < 0 {
			start = f
		} else if !loud && start >= 0 {
			closeBurst(f)
		}
	}
	if start >= 0 {
		closeBurst(frames)
	}
	return spans, crossings
}

// Detect reports each tone burst as a span of "speech", merging bursts
// closer together than the minimum silence (60 ms gaps by default stay split).
func (e *Engine) Detect(ctx context.Context, samples []float32, opts runner.DetectOptions) ([]runner.Span, error) {
	spans, _ := bursts(samples, opts.SampleRate)
	minSilence := float64(opts.MinSilenceMS) / 1000
	var out []runner.Span
	for _, sp := range spans {
		s, en := float64(sp[0])/100, float64(sp[1])/100
		if n := len(out); n > 0 && s-out[n-1].End < minSilence {
			out[n-1].End = en
			continue
		}
		out = append(out, runner.Span{Start: s, End: en})
	}
	return out, nil
}

// Diarize treats each pitch as a speaker: consecutive bursts of one pitch
// form a turn.
func (e *Engine) Diarize(ctx context.Context, samples []float32, opts runner.DiarizeOptions) ([]runner.Turn, error) {
	spans, hz := bursts(samples, opts.SampleRate)
	ids := map[int]int{}
	var out []runner.Turn
	for i, sp := range spans {
		key := int(math.Round(hz[i] / 100)) // 440 Hz and 880 Hz land in different bins
		id, ok := ids[key]
		if !ok {
			id = len(ids)
			ids[key] = id
		}
		s, en := float64(sp[0])/100, float64(sp[1])/100
		if n := len(out); n > 0 && out[n-1].Speaker == id {
			out[n-1].End = en
			continue
		}
		out = append(out, runner.Turn{Start: s, End: en, Speaker: id})
	}
	return out, nil
}

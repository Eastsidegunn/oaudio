package ref

import (
	"context"
	"strings"

	"github.com/eastsidegunn/oaudio/runner"
)

// OpenStream recognizes tone bursts as they arrive. A word is reported once
// its burst has ended, so the transcript only ever grows, as a streaming
// recognizer's does.
func (e *Engine) OpenStream(opts runner.StreamOptions) (runner.Stream, error) {
	if opts.SampleRate/100 == 0 {
		return nil, runner.BadRequest("sample rate too low")
	}
	if opts.Language == "xx" {
		return nil, runner.BadRequest("language %q is not supported", opts.Language)
	}
	return &stream{e: e, opts: opts}, nil
}

type stream struct {
	e       *Engine
	opts    runner.StreamOptions
	samples []float32
}

func (s *stream) words(final bool) string {
	audio := s.samples
	if !final {
		// A burst still sounding at the end is not a word yet.
		spans, _ := bursts(audio, s.opts.SampleRate)
		frame := s.opts.SampleRate / 100
		if n := len(spans); n > 0 && spans[n-1][1] == len(audio)/frame {
			audio = audio[:spans[n-1][0]*frame]
		}
	}
	var words []string
	s.e.Transcribe(context.Background(), audio, runner.TranscribeOptions{SampleRate: s.opts.SampleRate},
		func(seg runner.Segment) error { words = append(words, seg.Text); return nil })
	return strings.Join(words, " ")
}

func (s *stream) Accept(samples []float32) (string, error) {
	s.samples = append(s.samples, samples...)
	return s.words(false), nil
}

func (s *stream) Finish() (string, string, error) {
	lang := s.opts.Language
	if lang == "" {
		lang = "en"
	}
	return s.words(true), lang, nil
}

func (s *stream) Close() {}

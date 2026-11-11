package main

// Streaming recognition: sherpa-onnx's OnlineRecognizer, which decodes audio
// as it arrives and only ever extends its transcript. One recognizer serves
// both protocol paths: POST /v1/stream feeds it an utterance piece by piece,
// and POST /v1/transcribe feeds it a whole recording.

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/eastsidegunn/oaudio/runner"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/v2/sherpa_onnx"
)

type online struct {
	rec     *sherpa.OnlineRecognizer
	langs   []string // languages the manifest declares
	langOpt bool     // the model takes a per-stream "language" option
	tail    float64  // seconds of silence appended before the end, to flush the model's look-ahead
	piece   int      // samples per piece when a whole recording is fed
}

func loadOnline(m runner.Model, p paths) (runner.Engine, error) {
	cfg := sherpa.OnlineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: 80},
		ModelConfig: sherpa.OnlineModelConfig{
			Transducer: sherpa.OnlineTransducerModelConfig{
				Encoder: p.file("encoder"), Decoder: p.file("decoder"), Joiner: p.file("joiner"),
			},
			Tokens:     p.file("tokens"),
			NumThreads: threads(),
			Provider:   p.provider(),
			ModelType:  p.cfg["model_type"],
		},
		DecodingMethod: "greedy_search",
	}
	if v, err := strconv.Atoi(p.cfg["feature_dim"]); err == nil {
		cfg.FeatConfig.FeatureDim = v
	}
	rec := sherpa.NewOnlineRecognizer(&cfg)
	if rec == nil {
		return nil, errors.New("sherpa-onnx could not load the streaming model (see the log above)")
	}
	o := &online{rec: rec, langs: m.Languages, langOpt: p.cfg["language_option"] == "1",
		tail: float64(p.float("tail_seconds", 0.8)), piece: 16000 / 10}
	return o, nil
}

func (o *online) Info() runner.Info {
	return runner.Info{Engine: "sherpa-onnx", EngineVersion: sherpa.GetVersion(), Tasks: []string{"stt"}}
}

func (o *online) Close() error {
	sherpa.DeleteOnlineRecognizer(o.rec)
	return nil
}

// newStream starts a stream for one utterance in language (empty to detect).
func (o *online) newStream(language string) (*sherpa.OnlineStream, error) {
	if language != "" && !slices.Contains(o.langs, language) {
		return nil, runner.BadRequest("language %q is not supported by this model (it knows %s)", language, strings.Join(o.langs, ", "))
	}
	s := sherpa.NewOnlineStream(o.rec)
	if o.langOpt {
		if language == "" {
			language = "auto"
		}
		s.SetOption("language", language)
	}
	return s, nil
}

// decode runs the model over whatever input is ready.
func (o *online) decode(s *sherpa.OnlineStream) {
	for o.rec.IsReady(s) {
		o.rec.Decode(s)
	}
}

// finish appends the closing silence, marks the input finished and decodes
// the rest.
func (o *online) finish(s *sherpa.OnlineStream, rate int) {
	s.AcceptWaveform(rate, make([]float32, int(o.tail*float64(rate))))
	s.InputFinished()
	o.decode(s)
}

// tokenText joins the tokens of a result. sherpa-onnx's own text drops the
// spaces between Hangul syllables (it treats them like Chinese characters),
// though the model emits them as tokens; joining the tokens keeps them.
func tokenText(tokens []string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.Join(tokens, ""), "▁", " ")), " ")
}

// ---- POST /v1/stream ----

type onlineStream struct {
	o    *online
	s    *sherpa.OnlineStream
	rate int
	lang string
}

func (o *online) OpenStream(opts runner.StreamOptions) (runner.Stream, error) {
	s, err := o.newStream(opts.Language)
	if err != nil {
		return nil, err
	}
	return &onlineStream{o: o, s: s, rate: opts.SampleRate, lang: opts.Language}, nil
}

func (st *onlineStream) Accept(samples []float32) (string, error) {
	if len(samples) > 0 {
		st.s.AcceptWaveform(st.rate, samples)
		st.o.decode(st.s)
	}
	return tokenText(st.o.rec.GetResult(st.s).Tokens), nil
}

func (st *onlineStream) Finish() (string, string, error) {
	st.o.finish(st.s, st.rate)
	return tokenText(st.o.rec.GetResult(st.s).Tokens), st.lang, nil
}

func (st *onlineStream) Close() {
	if st.s != nil {
		sherpa.DeleteOnlineStream(st.s)
		st.s = nil
	}
}

// ---- POST /v1/transcribe ----

// Transcribe feeds a whole recording through one stream and reports a
// segment each time a sentence is complete, so that a long recording's
// segments arrive while the rest is still being recognized.
func (o *online) Transcribe(ctx context.Context, samples []float32, opts runner.TranscribeOptions, emit func(runner.Segment) error) (string, error) {
	if opts.Task == "translate" {
		return "", runner.BadRequest("this model transcribes only; it cannot translate")
	}
	s, err := o.newStream(opts.Language)
	if err != nil {
		return "", err
	}
	defer sherpa.DeleteOnlineStream(s)
	duration := float64(len(samples)) / float64(opts.SampleRate)
	done := 0 // tokens already emitted in segments
	flush := func(final bool) error {
		res := o.rec.GetResult(s)
		toks, times := res.Tokens, res.Timestamps
		if len(times) != len(toks) {
			times = nil
		}
		for done < len(toks) {
			end := sentenceEnd(toks, done, final)
			if end < 0 {
				return nil
			}
			seg := runner.Segment{Text: tokenText(toks[done:end])}
			segEnd := duration
			if times != nil {
				seg.Start = float64(times[done])
				if end < len(toks) {
					segEnd = float64(times[end])
				}
			}
			seg.End = math.Min(segEnd, duration)
			if seg.Start > seg.End {
				seg.Start = seg.End
			}
			if opts.WordTimestamps && times != nil {
				seg.Words = tokenWords(toks[done:end], times[done:end], 0, seg.End)
			}
			done = end
			if seg.Text == "" {
				continue
			}
			if err := emit(seg); err != nil {
				return err
			}
		}
		return nil
	}
	for i := 0; i < len(samples); i += o.piece {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		s.AcceptWaveform(opts.SampleRate, samples[i:min(i+o.piece, len(samples))])
		o.decode(s)
		if err := flush(false); err != nil {
			return "", err
		}
	}
	o.finish(s, opts.SampleRate)
	if err := flush(true); err != nil {
		return "", err
	}
	return opts.Language, nil
}

// sentenceEnd returns the index just past the next complete sentence in
// tokens from start: a sentence ends at a token ending in sentence-final
// punctuation that is followed by a further token. At the end of input the
// rest is a sentence. It returns -1 when no sentence is complete yet.
func sentenceEnd(tokens []string, start int, final bool) int {
	for i := start; i < len(tokens)-1; i++ {
		t := strings.TrimRight(tokens[i], " ")
		if t != "" && strings.ContainsAny(t[len(t)-1:], ".?!") || strings.HasSuffix(t, "。") || strings.HasSuffix(t, "？") || strings.HasSuffix(t, "！") {
			return i + 1
		}
	}
	if final {
		return len(tokens)
	}
	return -1
}

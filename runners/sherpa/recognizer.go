package main

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/eastsidegunn/oaudio/runner"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/v2/sherpa_onnx"
)

// recognizer runs any of sherpa-onnx's non-streaming recognition models. The
// manifest's family picks which model configuration the files go into;
// everything after loading is the same for all of them.
//
// These models take one utterance at a time and have no notion of segments,
// so a long recording is first cut at its silences with a voice activity
// detector, if the manifest ships one, and each piece becomes a segment.
type recognizer struct {
	cfg    sherpa.OfflineRecognizerConfig
	family string
	// recs holds one loaded recognizer per language hint ("" when the model
	// takes none, "auto" for detection). SenseVoice fixes its language when
	// it is created: changing it afterwards through the engine's SetConfig
	// also resets the feature settings the model asked for, and transcripts
	// change. So a second language means a second copy of the model, and
	// only the two most recently used are kept.
	recs  map[string]*sherpa.OfflineRecognizer
	order []string
	// langs are the languages the model can be told to expect. Nil for a
	// model that takes no such hint.
	langs map[string]bool
	vad   *sherpa.VadModelConfig
	// whole is the longest recording, in seconds, decoded in one piece.
	whole float64
}

// get returns the recognizer for a language hint, loading it on first use.
func (r *recognizer) get(hint string) (*sherpa.OfflineRecognizer, error) {
	if rec := r.recs[hint]; rec != nil {
		for i, h := range r.order {
			if h == hint {
				r.order = append(append(r.order[:i:i], r.order[i+1:]...), hint)
			}
		}
		return rec, nil
	}
	cfg := r.cfg
	if r.family == "sense-voice" {
		cfg.ModelConfig.SenseVoice.Language = hint
	}
	rec := sherpa.NewOfflineRecognizer(&cfg)
	if rec == nil {
		return nil, errors.New("sherpa-onnx could not load the model (see the messages above)")
	}
	if len(r.order) >= 2 {
		old := r.order[0]
		sherpa.DeleteOfflineRecognizer(r.recs[old])
		delete(r.recs, old)
		r.order = r.order[1:]
	}
	r.recs[hint] = rec
	r.order = append(r.order, hint)
	return rec, nil
}

func loadRecognizer(m runner.Model, p paths) (runner.Engine, error) {
	r := &recognizer{family: m.Family, whole: float64(p.float("whole_seconds", 30)), recs: map[string]*sherpa.OfflineRecognizer{}}
	mc := &r.cfg.ModelConfig
	featureDim := 80
	switch m.Family {
	case "sense-voice":
		mc.SenseVoice = sherpa.OfflineSenseVoiceModelConfig{Model: p.file("model"), Language: "auto"}
		if p.cfg["inverse_text_normalization"] != "false" {
			mc.SenseVoice.UseInverseTextNormalization = 1
		}
	case "transducer":
		mc.Transducer = sherpa.OfflineTransducerModelConfig{Encoder: p.file("encoder"), Decoder: p.file("decoder"), Joiner: p.file("joiner")}
		mc.ModelType = p.cfg["model_type"]
	case "qwen3-asr":
		mc.Qwen3ASR = sherpa.OfflineQwen3ASRModelConfig{
			ConvFrontend: p.file("conv_frontend"),
			Encoder:      p.file("encoder"),
			Decoder:      p.file("decoder"),
			Tokenizer:    p.file("tokenizer"),
			// The engine's own defaults. The total covers the audio as well
			// as the text, which is why this family decodes shorter pieces.
			MaxTotalLen:  512,
			MaxNewTokens: 128,
			Temperature:  1e-6,
			TopP:         0.8,
			Seed:         42,
		}
		featureDim = 128
	case "dolphin":
		mc.Dolphin = sherpa.OfflineDolphinModelConfig{Model: p.file("model")}
	case "moonshine":
		mc.Moonshine = sherpa.OfflineMoonshineModelConfig{
			Preprocessor:    p.file("preprocessor"),
			Encoder:         p.file("encoder"),
			UncachedDecoder: p.file("uncached_decoder"),
			CachedDecoder:   p.file("cached_decoder"),
			MergedDecoder:   p.file("merged_decoder"),
		}
	}
	mc.Tokens = p.file("tokens")
	mc.NumThreads = threads()
	mc.Provider = p.provider()
	r.cfg.FeatConfig = sherpa.FeatureConfig{SampleRate: 16000, FeatureDim: featureDim}
	r.cfg.DecodingMethod = "greedy_search"
	first := ""
	if m.Family == "sense-voice" {
		first = "auto"
	}
	if _, err := r.get(first); err != nil {
		return nil, err
	}
	if list := m.EngineConfig["language_hints"]; list != "" {
		r.langs = map[string]bool{}
		for _, l := range strings.Split(list, ",") {
			r.langs[strings.TrimSpace(l)] = true
		}
	}
	if file := p.file("vad"); file != "" {
		r.vad = &sherpa.VadModelConfig{SampleRate: 16000, NumThreads: 1, Provider: p.provider()}
		r.vad.SileroVad = sherpa.SileroVadModelConfig{
			Model:              file,
			Threshold:          0.5,
			MinSilenceDuration: 0.3,
			MinSpeechDuration:  0.25,
			WindowSize:         512,
			MaxSpeechDuration:  p.float("max_speech", 20),
		}
		d := sherpa.NewVoiceActivityDetector(r.vad, 60)
		if d == nil {
			return nil, errors.New("sherpa-onnx could not load the speech detector shipped with the model")
		}
		sherpa.DeleteVoiceActivityDetector(d)
	}
	return r, nil
}

func (r *recognizer) Info() runner.Info {
	return runner.Info{Engine: "sherpa-onnx", EngineVersion: sherpa.GetVersion(), Tasks: []string{"stt"}, SampleRate: 16000}
}

func (r *recognizer) Close() error {
	for _, rec := range r.recs {
		sherpa.DeleteOfflineRecognizer(rec)
	}
	return nil
}

func (r *recognizer) Transcribe(ctx context.Context, samples []float32, opts runner.TranscribeOptions, emit func(runner.Segment) error) (string, error) {
	if opts.Task == "translate" {
		return "", runner.BadRequest("this model transcribes only; it does not translate")
	}
	lang := strings.ToLower(opts.Language)
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	hint := ""
	if r.family == "sense-voice" {
		if lang != "" && !r.langs[lang] {
			return "", runner.BadRequest("this model does not recognise %q", opts.Language)
		}
		if hint = lang; hint == "" {
			hint = "auto"
		}
	}
	rec, err := r.get(hint)
	if err != nil {
		return "", err
	}
	rate := opts.SampleRate
	total := float64(len(samples)) / float64(rate)
	if total == 0 {
		return lang, nil
	}

	pieces := []runner.Span{{Start: 0, End: total}}
	if total > r.whole && r.vad != nil && rate == 16000 {
		spans, err := speechSpans(ctx, r.vad, samples)
		if err != nil {
			return "", err
		}
		pieces = joinSpans(spans, 1, r.whole)
	}

	detected := ""
	for _, piece := range pieces {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Round: seconds do not always map back to the sample they came
		// from, and a recording one sample short can come out as other words.
		from, to := int(math.Round(piece.Start*float64(rate))), int(math.Round(piece.End*float64(rate)))
		if to > len(samples) {
			to = len(samples)
		}
		if from >= to {
			continue
		}
		stream := sherpa.NewOfflineStream(rec)
		stream.AcceptWaveform(rate, samples[from:to])
		rec.Decode(stream)
		res := stream.GetResult()
		sherpa.DeleteOfflineStream(stream)
		if res == nil {
			continue
		}
		if detected == "" {
			detected = strings.Trim(res.Lang, "<|>")
		}
		text := strings.TrimSpace(res.Text)
		if text == "" {
			continue
		}
		seg := runner.Segment{Start: piece.Start, End: piece.End, Text: text}
		if opts.WordTimestamps {
			seg.Words = tokenWords(res.Tokens, res.Timestamps, piece.Start, piece.End)
		}
		if err := emit(seg); err != nil {
			return "", err
		}
	}
	if lang == "" {
		lang = detected
	}
	return lang, nil
}

// joinSpans merges neighbouring speech spans separated by less than gap
// seconds, as long as the result stays within limit seconds: a recognizer
// does better with a whole sentence than with its fragments.
func joinSpans(spans []runner.Span, gap, limit float64) []runner.Span {
	var out []runner.Span
	for _, s := range spans {
		if n := len(out); n > 0 && s.Start-out[n-1].End < gap && s.End-out[n-1].Start <= limit {
			out[n-1].End = s.End
			continue
		}
		out = append(out, s)
	}
	return out
}

// tokenWords builds word times from the engine's token times. A token that
// starts with a space or with the SentencePiece word mark begins a new word.
// It returns nil when the model reports no token times.
func tokenWords(tokens []string, times []float32, offset, end float64) []runner.Word {
	if len(tokens) == 0 || len(times) != len(tokens) {
		return nil
	}
	var words []runner.Word
	for i, tok := range tokens {
		start := math.Round((offset+float64(times[i]))*1000) / 1000
		fresh := strings.HasPrefix(tok, "▁") || strings.HasPrefix(tok, " ")
		piece := strings.TrimLeft(tok, "▁ ")
		if piece == "" {
			if fresh && len(words) > 0 && words[len(words)-1].Word != "" {
				words = append(words, runner.Word{Start: start})
			}
			continue
		}
		if fresh || len(words) == 0 {
			if n := len(words); n > 0 && words[n-1].Word == "" {
				words[n-1].Start = start
			} else {
				words = append(words, runner.Word{Start: start})
			}
		}
		words[len(words)-1].Word += piece
	}
	if n := len(words); n > 0 && words[n-1].Word == "" {
		words = words[:n-1]
	}
	for i := range words {
		words[i].End = end
		if i+1 < len(words) {
			words[i].End = words[i+1].Start
		}
	}
	return words
}

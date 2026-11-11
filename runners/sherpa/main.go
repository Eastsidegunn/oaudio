// Command oaudio-runner-sherpa-onnx runs ONNX speech models through
// sherpa-onnx (https://github.com/k2-fsa/sherpa-onnx, Apache-2.0) and
// ONNX Runtime: recognition (SenseVoice, Parakeet, Qwen3-ASR, Moonshine, and
// Nemotron streaming, which recognizes speech while it is spoken),
// Kokoro and Supertonic synthesis, Pocket TTS voice cloning, Silero voice
// activity detection, and pyannote speaker diarization.
//
// All of the modelling here is sherpa-onnx's work. This file only adapts it
// to the Oaudio runner contract.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/eastsidegunn/oaudio/runner"
	sherpa "github.com/k2-fsa/sherpa-onnx-go/v2/sherpa_onnx"
)

func threads() int {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	return n
}

// paths resolves engine_config file names inside the model directory.
type paths struct {
	dir string
	cfg map[string]string
}

func (p paths) file(key string) string {
	name := p.cfg[key]
	if name == "" {
		return ""
	}
	return filepath.Join(p.dir, filepath.FromSlash(name))
}

func (p paths) list(key string) string {
	var out []string
	for _, n := range strings.Split(p.cfg[key], ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, filepath.Join(p.dir, filepath.FromSlash(n)))
		}
	}
	return strings.Join(out, ",")
}

func (p paths) float(key string, def float32) float32 {
	if v, err := strconv.ParseFloat(p.cfg[key], 32); err == nil {
		return float32(v)
	}
	return def
}

func (p paths) provider() string {
	if v := p.cfg["provider"]; v != "" {
		return v
	}
	return "cpu"
}

func load(m runner.Model) (runner.Engine, error) {
	p := paths{dir: m.Dir, cfg: m.EngineConfig}
	switch m.Task + "/" + m.Family {
	case "tts/kokoro":
		return loadKokoro(m, p)
	case "tts/pocket":
		return loadPocket(p)
	case "tts/supertonic":
		return loadSupertonic(m, p)
	case "stt/sense-voice", "stt/transducer", "stt/qwen3-asr", "stt/dolphin", "stt/moonshine":
		return loadRecognizer(m, p)
	case "stt/online-transducer":
		return loadOnline(m, p)
	case "vad/silero":
		return loadVAD(p)
	case "diarize/pyannote":
		return loadDiarizer(p)
	}
	return nil, fmt.Errorf("this runner does not support task %q with model family %q", m.Task, m.Family)
}

// ---- synthesis ----

type tts struct {
	tts    *sherpa.OfflineTts
	voices []runner.Voice
	sid    map[string]int
	def    string
	clone  bool
	words  *lexicon

	// Supertonic takes the language of the text as a per-request option.
	langs       map[string]bool
	defaultLang string
	steps       int
}

func loadKokoro(m runner.Model, p paths) (runner.Engine, error) {
	var cfg sherpa.OfflineTtsConfig
	cfg.Model.Kokoro = sherpa.OfflineTtsKokoroModelConfig{
		Model:       p.file("model"),
		Voices:      p.file("voices"),
		Tokens:      p.file("tokens"),
		Lexicon:     p.list("lexicon"),
		LengthScale: 1,
	}
	cfg.Model.NumThreads = threads()
	cfg.Model.Provider = p.provider()
	cfg.MaxNumSentences = 1
	engine := sherpa.NewOfflineTts(&cfg)
	if engine == nil {
		return nil, errors.New("sherpa-onnx could not load the model (see the messages above)")
	}
	e := &tts{tts: engine, voices: m.Voices, sid: map[string]int{}, def: m.DefaultVoice}
	// The manifest lists voices in speaker-id order.
	if n := engine.NumSpeakers(); n != len(m.Voices) {
		fmt.Fprintf(os.Stderr, "warning: model has %d speakers, manifest lists %d voices\n", n, len(m.Voices))
	}
	for i, v := range m.Voices {
		e.sid[strings.ToLower(v.ID)] = i
	}
	// sherpa-onnx drops words that are not in the lexicon. Knowing the
	// lexicon lets the runner respell such words from ones that are.
	if lex := p.file("oov_lexicon"); lex != "" {
		words, err := loadLexicon(lex)
		if err != nil {
			return nil, err
		}
		e.words = words
	}
	return e, nil
}

func loadSupertonic(m runner.Model, p paths) (runner.Engine, error) {
	var cfg sherpa.OfflineTtsConfig
	cfg.Model.Supertonic = sherpa.OfflineTtsSupertonicModelConfig{
		DurationPredictor: p.file("duration_predictor"),
		TextEncoder:       p.file("text_encoder"),
		VectorEstimator:   p.file("vector_estimator"),
		Vocoder:           p.file("vocoder"),
		TtsJson:           p.file("tts_json"),
		UnicodeIndexer:    p.file("unicode_indexer"),
		VoiceStyle:        p.file("voice_style"),
	}
	cfg.Model.NumThreads = threads()
	cfg.Model.Provider = p.provider()
	engine := sherpa.NewOfflineTts(&cfg)
	if engine == nil {
		return nil, errors.New("sherpa-onnx could not load the model (see the messages above)")
	}
	e := &tts{tts: engine, voices: m.Voices, sid: map[string]int{}, def: m.DefaultVoice, langs: map[string]bool{}}
	for i, v := range m.Voices {
		e.sid[strings.ToLower(v.ID)] = i
	}
	for _, l := range strings.Split(m.EngineConfig["languages"], ",") {
		if l = strings.TrimSpace(l); l != "" {
			e.langs[l] = true
		}
	}
	e.defaultLang = m.EngineConfig["default_language"]
	if e.defaultLang == "" {
		e.defaultLang = "en"
	}
	if e.steps, _ = strconv.Atoi(m.EngineConfig["num_steps"]); e.steps <= 0 {
		e.steps = 8
	}
	return e, nil
}

// guessLanguage picks the language of text from its script when the request
// does not say. Only scripts that belong to one supported language can be
// told apart this way; everything else gets the model's default.
func guessLanguage(text, def string) string {
	counts := map[string]int{}
	for _, r := range text {
		switch {
		case unicode.In(r, unicode.Hangul):
			counts["ko"]++
		case unicode.In(r, unicode.Hiragana, unicode.Katakana):
			counts["ja"]++
		case unicode.In(r, unicode.Arabic):
			counts["ar"]++
		case unicode.In(r, unicode.Devanagari):
			counts["hi"]++
		case unicode.In(r, unicode.Greek):
			counts["el"]++
		}
	}
	best, n := def, 0
	for lang, c := range counts {
		if c > n {
			best, n = lang, c
		}
	}
	return best
}

func loadPocket(p paths) (runner.Engine, error) {
	var cfg sherpa.OfflineTtsConfig
	cfg.Model.Pocket = sherpa.OfflineTtsPocketModelConfig{
		LmFlow:                      p.file("lm_flow"),
		LmMain:                      p.file("lm_main"),
		Encoder:                     p.file("encoder"),
		Decoder:                     p.file("decoder"),
		TextConditioner:             p.file("text_conditioner"),
		VocabJson:                   p.file("vocab"),
		TokenScoresJson:             p.file("token_scores"),
		VoiceEmbeddingCacheCapacity: 16,
	}
	cfg.Model.NumThreads = threads()
	cfg.Model.Provider = p.provider()
	engine := sherpa.NewOfflineTts(&cfg)
	if engine == nil {
		return nil, errors.New("sherpa-onnx could not load the model (see the messages above)")
	}
	return &tts{tts: engine, clone: true}, nil
}

func (e *tts) Info() runner.Info {
	return runner.Info{Engine: "sherpa-onnx", EngineVersion: sherpa.GetVersion(), Tasks: []string{"tts"}, SampleRate: e.tts.SampleRate()}
}

func (e *tts) Close() error {
	sherpa.DeleteOfflineTts(e.tts)
	return nil
}

func (e *tts) Voices(context.Context) ([]runner.Voice, error) { return e.voices, nil }

func (e *tts) Synthesize(ctx context.Context, req runner.SynthesisRequest, emit func([]byte) error) error {
	speed := float32(req.Speed)
	if speed <= 0 {
		speed = 1
	}
	cfg := &sherpa.GenerationConfig{Speed: speed}
	text := req.Text
	switch {
	case e.clone:
		if req.Reference == nil {
			return runner.BadRequest("this model has no built-in voices: create one with `oaudio voice create` and pass it as the voice")
		}
		wave := sherpa.ReadWave(req.Reference.AudioPath)
		if wave == nil || len(wave.Samples) == 0 {
			return fmt.Errorf("could not read the reference recording %s", req.Reference.AudioPath)
		}
		cfg.ReferenceAudio, cfg.ReferenceSampleRate = wave.Samples, wave.SampleRate
		cfg.ReferenceText = req.Reference.Text
	case req.Reference != nil:
		return runner.BadRequest("this model cannot clone a voice; use one of its built-in voices")
	default:
		name := req.Voice
		if name == "" {
			name = e.def
		}
		sid, ok := e.sid[strings.ToLower(name)]
		if !ok {
			return runner.BadRequest("unknown voice %q", req.Voice)
		}
		cfg.Sid = sid
		if e.words != nil {
			text = e.words.respell(text)
		}
		if e.langs != nil {
			lang := strings.ToLower(req.Language)
			if i := strings.IndexAny(lang, "-_"); i > 0 {
				lang = lang[:i]
			}
			if lang == "" {
				lang = guessLanguage(text, e.defaultLang)
			}
			if !e.langs[lang] {
				return runner.BadRequest("this model does not speak %q", req.Language)
			}
			cfg.NumSteps = e.steps
			cfg.Extra, _ = json.Marshal(map[string]string{"lang": lang})
		}
	}
	var emitErr error
	audio := e.tts.GenerateWithConfig(text, cfg, func(samples []float32, _ float32) bool {
		if emitErr = emit(runner.PCM16(samples)); emitErr != nil {
			return false
		}
		return ctx.Err() == nil // false stops generation for a client that left
	})
	switch {
	case emitErr != nil:
		return emitErr
	case ctx.Err() != nil:
		return ctx.Err()
	case audio == nil:
		return errors.New("sherpa-onnx produced no audio")
	}
	return nil
}

// ---- voice activity detection ----

type vad struct {
	base sherpa.VadModelConfig
}

func loadVAD(p paths) (runner.Engine, error) {
	v := &vad{}
	v.base.SileroVad = sherpa.SileroVadModelConfig{
		Model:              p.file("model"),
		Threshold:          p.float("threshold", 0.5),
		MinSilenceDuration: p.float("min_silence", 0.5),
		MinSpeechDuration:  p.float("min_speech", 0.25),
		WindowSize:         512,
		MaxSpeechDuration:  p.float("max_speech", 30),
	}
	v.base.SampleRate = 16000
	v.base.NumThreads = 1
	v.base.Provider = p.provider()
	// Build one detector now so a bad model fails at load, not on first use.
	d := sherpa.NewVoiceActivityDetector(&v.base, 60)
	if d == nil {
		return nil, errors.New("sherpa-onnx could not load the model (see the messages above)")
	}
	sherpa.DeleteVoiceActivityDetector(d)
	return v, nil
}

func (v *vad) Info() runner.Info {
	return runner.Info{Engine: "sherpa-onnx", EngineVersion: sherpa.GetVersion(), Tasks: []string{"vad"}}
}

func (v *vad) Close() error { return nil }

func (v *vad) Detect(ctx context.Context, samples []float32, opts runner.DetectOptions) ([]runner.Span, error) {
	if opts.SampleRate != 16000 {
		return nil, runner.BadRequest("this model needs 16 kHz audio, got %d Hz", opts.SampleRate)
	}
	cfg := v.base
	if opts.Threshold > 0 {
		cfg.SileroVad.Threshold = float32(opts.Threshold)
	}
	if opts.MinSilenceMS > 0 {
		cfg.SileroVad.MinSilenceDuration = float32(opts.MinSilenceMS) / 1000
	}
	if opts.MinSpeechMS > 0 {
		cfg.SileroVad.MinSpeechDuration = float32(opts.MinSpeechMS) / 1000
	}
	return speechSpans(ctx, &cfg, samples)
}

// speechSpans runs a detector over 16 kHz samples. A detector carries state
// from one window to the next, so each call builds its own.
func speechSpans(ctx context.Context, cfg *sherpa.VadModelConfig, samples []float32) ([]runner.Span, error) {
	d := sherpa.NewVoiceActivityDetector(cfg, float32(len(samples))/16000+30)
	if d == nil {
		return nil, errors.New("sherpa-onnx could not create a detector")
	}
	defer sherpa.DeleteVoiceActivityDetector(d)

	var spans []runner.Span
	drain := func() {
		for !d.IsEmpty() {
			seg := d.Front()
			d.Pop()
			spans = append(spans, runner.Span{
				Start: float64(seg.Start) / 16000,
				End:   float64(seg.Start+int64(len(seg.Samples))) / 16000,
			})
		}
	}
	const window = 512
	for i := 0; i+window <= len(samples); i += window {
		if i%(window*256) == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d.AcceptWaveform(samples[i : i+window])
		drain()
	}
	d.Flush()
	drain()
	return spans, nil
}

// ---- speaker diarization ----

type diarizer struct {
	sd  *sherpa.OfflineSpeakerDiarization
	cfg sherpa.OfflineSpeakerDiarizationConfig
}

func loadDiarizer(p paths) (runner.Engine, error) {
	d := &diarizer{}
	d.cfg.Segmentation.Pyannote.Model = p.file("segmentation")
	d.cfg.Segmentation.NumThreads = threads()
	d.cfg.Segmentation.Provider = p.provider()
	d.cfg.Embedding.Model = p.file("embedding")
	d.cfg.Embedding.NumThreads = threads()
	d.cfg.Embedding.Provider = p.provider()
	d.cfg.Clustering.Threshold = p.float("threshold", 0.5)
	d.cfg.MinDurationOn = p.float("min_duration_on", 0.3)
	d.cfg.MinDurationOff = p.float("min_duration_off", 0.5)
	d.sd = sherpa.NewOfflineSpeakerDiarization(&d.cfg)
	if d.sd == nil {
		return nil, errors.New("sherpa-onnx could not load the models (see the messages above)")
	}
	return d, nil
}

func (d *diarizer) Info() runner.Info {
	return runner.Info{Engine: "sherpa-onnx", EngineVersion: sherpa.GetVersion(), Tasks: []string{"diarize"}}
}

func (d *diarizer) Close() error {
	sherpa.DeleteOfflineSpeakerDiarization(d.sd)
	return nil
}

func (d *diarizer) Diarize(ctx context.Context, samples []float32, opts runner.DiarizeOptions) ([]runner.Turn, error) {
	if want := d.sd.SampleRate(); opts.SampleRate != want {
		return nil, runner.BadRequest("this model needs %d Hz audio, got %d Hz", want, opts.SampleRate)
	}
	if len(samples) == 0 {
		return nil, nil
	}
	cfg := d.cfg
	cfg.Clustering.NumClusters = opts.NumSpeakers
	if opts.NumSpeakers <= 0 {
		cfg.Clustering.NumClusters = -1
		if opts.Threshold > 0 {
			cfg.Clustering.Threshold = float32(opts.Threshold)
		}
	}
	d.sd.SetConfig(&cfg)
	segs := d.sd.Process(samples)
	// Number speakers in order of first appearance, whatever ids clustering chose.
	order := map[int]int{}
	turns := make([]runner.Turn, 0, len(segs))
	for _, s := range segs {
		id, ok := order[s.Speaker]
		if !ok {
			id = len(order)
			order[s.Speaker] = id
		}
		turns = append(turns, runner.Turn{Start: float64(s.Start), End: float64(s.End), Speaker: id})
	}
	return turns, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("oaudio-runner-sherpa-onnx, sherpa-onnx", sherpa.GetVersion())
		return
	}
	runner.Main(os.Args[1:], load)
}

// Command oaudio-runner-whisper.cpp runs Whisper models in GGML format
// through whisper.cpp (https://github.com/ggml-org/whisper.cpp, MIT), using
// Metal on Apple Silicon.
//
// All of the speech recognition here is whisper.cpp's work. This file only
// adapts it to the Oaudio runner contract.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/eastsidegunn/oaudio/runner"
	whisper "github.com/ggerganov/whisper.cpp/bindings/go"
)

// engineVersion is the whisper.cpp tag this binary was built from, set by the Makefile.
var engineVersion = "unknown"

// beamSize matches the default of whisper.cpp's own command-line tool, so a
// transcript from Oaudio is the transcript whisper-cli would give.
const beamSize = 5

type engine struct {
	ctx          *whisper.Context
	multilingual bool
}

func load(m runner.Model) (runner.Engine, error) {
	file := m.EngineConfig["model"]
	if file == "" {
		if len(m.Source.Files) != 1 {
			return nil, errors.New("manifest must list exactly one GGML file, or name it in engine_config.model")
		}
		file = m.Source.Files[0].Path
	}
	path := filepath.Join(m.Dir, filepath.FromSlash(file))
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	ctx := whisper.Whisper_init(path)
	if ctx == nil {
		return nil, fmt.Errorf("whisper.cpp could not load %s", path)
	}
	return &engine{ctx: ctx, multilingual: ctx.Whisper_is_multilingual() != 0}, nil
}

func (e *engine) Info() runner.Info {
	return runner.Info{Engine: "whisper.cpp", EngineVersion: engineVersion, Tasks: []string{"stt"}}
}

func (e *engine) Close() error {
	e.ctx.Whisper_free()
	return nil
}

func (e *engine) Transcribe(ctx context.Context, samples []float32, opts runner.TranscribeOptions, emit func(runner.Segment) error) (string, error) {
	if opts.SampleRate != 16000 {
		return "", runner.BadRequest("whisper needs 16 kHz audio, got %d Hz", opts.SampleRate)
	}
	lang := strings.ToLower(opts.Language)
	params := e.ctx.Whisper_full_default_params(whisper.SAMPLING_BEAM_SEARCH)
	params.SetBeamSize(beamSize)
	params.SetPrintSpecial(false)
	params.SetPrintProgress(false)
	params.SetPrintRealtime(false)
	params.SetPrintTimestamps(false)
	switch {
	case !e.multilingual:
		if lang != "" && lang != "en" {
			return "", runner.BadRequest("this model only understands English, not %q", opts.Language)
		}
		if opts.Task == "translate" {
			return "", runner.BadRequest("this model is English-only and cannot translate")
		}
		lang = "en"
	case lang == "":
		params.SetLanguage(-1) // detect
	default:
		id := e.ctx.Whisper_lang_id(lang)
		if id < 0 {
			return "", runner.BadRequest("language %q is not supported by Whisper", opts.Language)
		}
		params.SetLanguage(id)
	}
	params.SetTranslate(opts.Task == "translate")
	if opts.Prompt != "" {
		params.SetInitialPrompt(opts.Prompt)
	}
	params.SetTemperature(float32(opts.Temperature))
	params.SetTokenTimestamps(opts.WordTimestamps)
	threads := runtime.NumCPU()
	if threads > 8 {
		threads = 8
	}
	params.SetThreads(threads)

	if len(samples) == 0 {
		return lang, nil
	}
	eot := e.ctx.Whisper_token_eot()
	var emitErr error
	// whisper.cpp calls this as segments are finalised, so they reach the
	// client while later audio is still being decoded.
	onSegments := func(n int) {
		total := e.ctx.Whisper_full_n_segments()
		for i := total - n; i < total && emitErr == nil; i++ {
			seg := runner.Segment{
				Start: float64(e.ctx.Whisper_full_get_segment_t0(i)) / 100,
				End:   float64(e.ctx.Whisper_full_get_segment_t1(i)) / 100,
				Text:  e.ctx.Whisper_full_get_segment_text(i),
			}
			if opts.WordTimestamps {
				seg.Words = e.words(i, eot)
			}
			emitErr = emit(seg)
		}
	}
	// Checked before each 30-second window: returning false stops work for a
	// client that has gone away.
	keepGoing := func() bool { return ctx.Err() == nil && emitErr == nil }
	if err := e.ctx.Whisper_full(params, samples, keepGoing, onSegments, nil); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if emitErr != nil {
			return "", emitErr
		}
		return "", err
	}
	if emitErr != nil {
		return "", emitErr
	}
	if lang == "" {
		lang = whisper.Whisper_lang_str(e.ctx.Whisper_full_lang_id())
	}
	return lang, nil
}

// words merges the sub-word tokens of segment i into words. In Whisper's
// vocabulary a token that starts a new word begins with a space; tokens at or
// above the end-of-text id are control tokens, not text.
func (e *engine) words(i int, eot whisper.Token) []runner.Word {
	var out []runner.Word
	for j := 0; j < e.ctx.Whisper_full_n_tokens(i); j++ {
		data := e.ctx.Whisper_full_get_token_data(i, j)
		if data.Id() >= eot {
			continue
		}
		raw := e.ctx.Whisper_full_get_token_text(i, j)
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		start, end := float64(data.T0())/100, float64(data.T1())/100
		if strings.HasPrefix(raw, " ") || len(out) == 0 {
			out = append(out, runner.Word{Word: text, Start: start, End: end})
		} else {
			last := &out[len(out)-1]
			last.Word += text
			last.End = end
		}
	}
	return out
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("oaudio-runner-whisper.cpp, whisper.cpp", engineVersion)
		return
	}
	runner.Main(os.Args[1:], load)
}

package main

// Synthesis through the worker: `synthesize` requests answered with `chunk`
// lines of PCM as the model makes them, and `add_voice` for the reference
// recordings of cloned voices.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/eastsidegunn/oaudio/runner"
)

// workerModel is the part of the model information in `ready` this runner reads.
type workerModel struct {
	Task            string `json:"task"`
	SampleRate      int    `json:"sample_rate"`
	VoiceRecordings bool   `json:"voice_recordings"` // takes a WAVE file as a voice
	Voices          []struct {
		Name string `json:"name"`
	} `json:"voices"`
}

type synthesis struct {
	voices     []runner.Voice
	defVoice   string
	langs      []string
	recordings bool // can speak in the voice of a reference recording
	speed      bool // takes the speed option

	// The worker keeps every voice added to it. A reference recording is
	// added once, under a name of its own, and reused while the file is
	// unchanged.
	added map[string]string
}

// newSynthesis checks the manifest against what the worker says of the
// model, so that a manifest that does not describe its file fails at load
// rather than on the first request.
func newSynthesis(m runner.Model, wm *workerModel) (*synthesis, error) {
	if wm == nil || wm.Task != "synthesis" {
		return nil, fmt.Errorf("speechcpp: %s is not a synthesis model", m.EngineConfig["model"])
	}
	if wm.SampleRate != m.Audio.SampleRate {
		return nil, fmt.Errorf("speechcpp: the model speaks at %d Hz, the manifest says %d", wm.SampleRate, m.Audio.SampleRate)
	}
	builtin := map[string]bool{}
	for _, v := range wm.Voices {
		builtin[v.Name] = true
	}
	for _, v := range m.Voices {
		if !builtin[v.ID] {
			return nil, fmt.Errorf("speechcpp: the manifest lists voice %q, which the model does not have", v.ID)
		}
	}
	s := &synthesis{
		voices:     m.Voices,
		defVoice:   m.DefaultVoice,
		recordings: wm.VoiceRecordings,
		speed:      m.EngineConfig["speed"] == "true",
		added:      map[string]string{},
	}
	if v := m.EngineConfig["languages"]; v != "" {
		s.langs = strings.Split(v, ",")
	}
	return s, nil
}

func (e *engine) Voices(context.Context) ([]runner.Voice, error) {
	if e.synth == nil {
		return nil, nil
	}
	return e.synth.voices, nil
}

func (e *engine) Synthesize(ctx context.Context, req runner.SynthesisRequest, emit func([]byte) error) error {
	s := e.synth
	if s == nil {
		return runner.BadRequest("this model recognizes speech; it does not speak")
	}
	lang := strings.ToLower(req.Language)
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	if lang != "" && len(s.langs) > 0 && !contains(s.langs, lang) {
		return runner.BadRequest("language %q: this model speaks %s", req.Language, strings.Join(s.langs, ", "))
	}
	voice, err := e.voiceFor(ctx, req)
	if err != nil {
		return err
	}

	e.next++
	id := strconv.Itoa(e.next)
	body := map[string]any{"type": "synthesize", "id": id, "text": req.Text, "voice": voice}
	if lang != "" {
		body["language"] = lang
	}
	if req.Speed != 0 && req.Speed != 1 {
		if !s.speed {
			return runner.BadRequest("this model has no control of its speed")
		}
		body["speed"] = req.Speed
	}
	if err := e.send(body); err != nil {
		return e.failure(err)
	}

	next, cancelled := 0, false
	var emitErr error
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
				return e.stopped()
			}
			if msg.ID != id {
				if msg.ID == "" && msg.Type == "error" {
					return fmt.Errorf("speechcpp: %s", errText(msg.Error))
				}
				continue
			}
			switch msg.Type {
			case "chunk":
				if msg.Seq != next {
					return fmt.Errorf("speechcpp: chunk %d came after chunk %d", msg.Seq, next-1)
				}
				next++
				if emitErr != nil {
					continue // the caller is gone; the cancel below ends the request
				}
				pcm, err := base64.StdEncoding.DecodeString(msg.PCM)
				if err != nil {
					return fmt.Errorf("speechcpp: chunk %d: %w", msg.Seq, err)
				}
				if emitErr = emit(pcm); emitErr != nil && !cancelled {
					e.send(map[string]any{"type": "cancel", "id": id})
					cancelled = true
				}
			case "progress":
			case "end":
				return emitErr
			case "cancelled":
				if emitErr != nil {
					return emitErr
				}
				return context.Canceled
			case "error":
				return workerError(msg.Error)
			}
		}
	}
}

// voiceFor names the worker voice a request speaks in: a built-in voice, or
// a reference recording added to the worker on its first use.
func (e *engine) voiceFor(ctx context.Context, req runner.SynthesisRequest) (string, error) {
	s := e.synth
	if req.Reference != nil {
		if !s.recordings {
			return "", runner.BadRequest("this model speaks only in its own voices; it cannot clone")
		}
		st, err := os.Stat(req.Reference.AudioPath)
		if err != nil {
			return "", fmt.Errorf("reference recording: %w", err)
		}
		key := fmt.Sprintf("%s\x00%d\x00%d", req.Reference.AudioPath, st.Size(), st.ModTime().UnixNano())
		if name, ok := s.added[key]; ok {
			return name, nil
		}
		name := fmt.Sprintf("oaudio-%d", len(s.added)+1)
		if err := e.addVoice(ctx, name, req.Reference.AudioPath); err != nil {
			return "", err
		}
		s.added[key] = name
		return name, nil
	}
	name := req.Voice
	if name == "" {
		name = s.defVoice
	}
	for _, v := range s.voices {
		if strings.EqualFold(v.ID, name) {
			return v.ID, nil
		}
	}
	if len(s.voices) == 0 {
		return "", runner.BadRequest("this model has no built-in voices: create one with `oaudio voice create` and pass it as the voice")
	}
	ids := make([]string, len(s.voices))
	for i, v := range s.voices {
		ids[i] = v.ID
	}
	return "", runner.BadRequest("unknown voice %q; this model has %s", req.Voice, strings.Join(ids, ", "))
}

// addVoice gives the worker a reference recording under name. It cannot be
// cancelled once it runs, and takes under a second for ten seconds of audio.
func (e *engine) addVoice(ctx context.Context, name, path string) error {
	e.next++
	id := strconv.Itoa(e.next)
	if err := e.send(map[string]any{"type": "add_voice", "id": id, "name": name, "path": path}); err != nil {
		return e.failure(err)
	}
	for msg := range e.lines {
		if msg.ID != id {
			if msg.ID == "" && msg.Type == "error" {
				return fmt.Errorf("speechcpp: %s", errText(msg.Error))
			}
			continue
		}
		switch msg.Type {
		case "end":
			return nil
		case "error":
			return workerError(msg.Error)
		}
	}
	return e.stopped()
}

// stopped is the error of a request whose worker went away. The worker's
// lines close just before it is marked dead, so this waits for the latter.
func (e *engine) stopped() error {
	<-e.dead
	return fmt.Errorf("speechcpp stopped: %v", e.werr)
}

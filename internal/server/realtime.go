package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eastsidegunn/oaudio/internal/audio"
	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/internal/ws"
	"github.com/eastsidegunn/oaudio/runner"
)

// The realtime endpoint implements the transcription sessions of OpenAI's
// Realtime API over WebSocket: the client streams microphone audio, the
// server decides where each utterance ends and sends back its transcript.
//
// Conversation sessions, where a model also answers, need a language model
// and are not part of a speech runtime.

const (
	rtRate         = 24000 // the Realtime API's PCM rate
	rtTick         = 200 * time.Millisecond
	rtMaxTurn      = 30.0 // seconds; a longer utterance is cut here
	rtKeepSilence  = 1.0  // seconds of trailing audio kept while nobody speaks
	rtDefaultVADMS = 500
)

type rtTurnDetection struct {
	Type              string   `json:"type"`
	Threshold         *float64 `json:"threshold,omitempty"`
	PrefixPaddingMS   *int     `json:"prefix_padding_ms,omitempty"`
	SilenceDurationMS *int     `json:"silence_duration_ms,omitempty"`
}

type rtSession struct {
	Type  string `json:"type"`
	Audio struct {
		Input struct {
			Format *struct {
				Type string `json:"type"`
				Rate int    `json:"rate,omitempty"`
			} `json:"format,omitempty"`
			Transcription *struct {
				Model    string `json:"model,omitempty"`
				Language string `json:"language,omitempty"`
				Prompt   string `json:"prompt,omitempty"`
			} `json:"transcription,omitempty"`
			// A pointer to raw JSON tells "absent" from an explicit null,
			// which turns turn detection off.
			TurnDetection *json.RawMessage `json:"turn_detection,omitempty"`
		} `json:"input"`
	} `json:"audio"`
}

type rtUtterance struct {
	itemID  string
	samples []float32 // at rtRate
}

type realtime struct {
	s    *Server
	conn *ws.Conn
	ctx  context.Context

	mu       sync.Mutex
	eventSeq int
	model    *manifest.Manifest // transcription model
	language string
	prompt   string
	vadModel *manifest.Manifest // nil: loudness-based detection
	detect   bool               // server-side turn detection on
	thresh   float64
	prefixMS int
	silentMS int

	buf      []float32 // audio not yet committed, at rtRate
	bufStart int       // absolute sample index of buf[0]
	speaking bool
	itemID   string // item of the utterance in progress
	lastItem string
	dirty    bool // audio arrived since the last detection pass
	queue    chan rtUtterance
}

func newID(prefix string) string {
	var b [8]byte
	rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

func (rt *realtime) send(event map[string]any) {
	rt.mu.Lock()
	rt.eventSeq++
	event["event_id"] = fmt.Sprintf("event_%d", rt.eventSeq)
	rt.mu.Unlock()
	data, _ := json.Marshal(event)
	rt.conn.WriteText(data)
}

func (rt *realtime) sendError(code, message, clientEvent string) {
	e := map[string]any{"type": "invalid_request_error", "code": code, "message": message}
	if clientEvent != "" {
		e["event_id"] = clientEvent
	}
	rt.send(map[string]any{"type": "error", "error": e})
}

// sessionObject describes the session as the client should see it.
func (rt *realtime) sessionObject() map[string]any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	transcription := map[string]any{}
	if rt.model != nil {
		transcription["model"] = rt.model.Ref()
	}
	if rt.language != "" {
		transcription["language"] = rt.language
	}
	if rt.prompt != "" {
		transcription["prompt"] = rt.prompt
	}
	var turn any
	if rt.detect {
		turn = map[string]any{"type": "server_vad", "threshold": rt.thresh, "prefix_padding_ms": rt.prefixMS, "silence_duration_ms": rt.silentMS}
	}
	return map[string]any{
		"type": "transcription", "object": "realtime.transcription_session", "id": "sess_oaudio",
		"audio": map[string]any{"input": map[string]any{
			"format":         map[string]any{"type": "audio/pcm", "rate": rtRate},
			"transcription":  transcription,
			"turn_detection": turn,
		}},
	}
}

func (s *Server) handleRealtime(w http.ResponseWriter, r *http.Request) {
	conn, err := ws.Accept(w, r)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer conn.Close()

	rt := &realtime{
		s: s, conn: conn, ctx: ctx,
		detect: true, thresh: 0.5, prefixMS: 300, silentMS: rtDefaultVADMS,
		queue: make(chan rtUtterance, 64),
	}
	// The model may come from the URL, as the SDK's connect(model=...) sends
	// it, or later from session.update.
	if name := r.URL.Query().Get("model"); name != "" {
		m, err := s.ready(name, manifest.TaskSTT)
		if err != nil {
			rt.sendError("model_not_found", err.Error(), "")
			conn.CloseWith(1008, "model not available")
			return
		}
		rt.model = m
	}
	// Prefer a real voice activity detection model when exactly one is installed.
	if m, err := s.defaultModel(manifest.TaskVAD); err == nil {
		rt.vadModel = m
	}
	vadName := "loudness"
	if rt.vadModel != nil {
		vadName = rt.vadModel.Ref()
	}
	s.log.Info("realtime session opened", "vad", vadName)
	rt.send(map[string]any{"type": "session.created", "session": rt.sessionObject()})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); rt.transcribeLoop() }()
	go func() { defer wg.Done(); rt.detectLoop() }()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		rt.handle(data)
	}
	cancel()
	wg.Wait()
	s.log.Info("realtime session closed")
}

func (rt *realtime) handle(data []byte) {
	var ev struct {
		Type    string          `json:"type"`
		EventID string          `json:"event_id"`
		Audio   string          `json:"audio"`
		Session json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		rt.sendError("invalid_json", "could not parse the event: "+err.Error(), "")
		return
	}
	switch ev.Type {
	case "session.update":
		rt.updateSession(ev.Session, ev.EventID)
	case "input_audio_buffer.append":
		raw, err := base64.StdEncoding.DecodeString(ev.Audio)
		if err != nil {
			rt.sendError("invalid_audio", "audio must be base64-encoded 16-bit PCM at 24 kHz", ev.EventID)
			return
		}
		rt.mu.Lock()
		rt.buf = append(rt.buf, audio.S16ToFloat(raw[:len(raw)&^1])...)
		rt.dirty = true
		rt.mu.Unlock()
	case "input_audio_buffer.commit":
		rt.mu.Lock()
		if len(rt.buf) == 0 {
			rt.mu.Unlock()
			rt.sendError("input_audio_buffer_commit_empty", "the input audio buffer is empty", ev.EventID)
			return
		}
		u := rtUtterance{itemID: rt.itemID, samples: rt.buf}
		if u.itemID == "" {
			u.itemID = newID("item")
		}
		rt.bufStart += len(rt.buf)
		rt.buf, rt.speaking, rt.itemID = nil, false, ""
		rt.mu.Unlock()
		rt.commit(u)
	case "input_audio_buffer.clear":
		rt.mu.Lock()
		rt.bufStart += len(rt.buf)
		rt.buf, rt.speaking, rt.itemID = nil, false, ""
		rt.mu.Unlock()
		rt.send(map[string]any{"type": "input_audio_buffer.cleared"})
	default:
		rt.sendError("unsupported_event", fmt.Sprintf("event type %q is not supported; this server implements transcription sessions", ev.Type), ev.EventID)
	}
}

func (rt *realtime) updateSession(raw json.RawMessage, clientEvent string) {
	var sess rtSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		rt.sendError("invalid_session", "could not parse session: "+err.Error(), clientEvent)
		return
	}
	if sess.Type != "" && sess.Type != "transcription" {
		rt.sendError("unsupported_session_type", `only "transcription" sessions are supported; conversation sessions need a language model`, clientEvent)
		return
	}
	in := sess.Audio.Input
	if in.Format != nil && (in.Format.Type != "audio/pcm" || (in.Format.Rate != 0 && in.Format.Rate != rtRate)) {
		rt.sendError("unsupported_format", "input audio must be audio/pcm at 24000 Hz", clientEvent)
		return
	}
	var model *manifest.Manifest
	if in.Transcription != nil && in.Transcription.Model != "" {
		m, err := rt.s.ready(in.Transcription.Model, manifest.TaskSTT)
		if err != nil {
			rt.sendError("model_not_found", err.Error(), clientEvent)
			return
		}
		model = m
	}
	var td *rtTurnDetection
	turnOff := false
	if in.TurnDetection != nil {
		if string(*in.TurnDetection) == "null" {
			turnOff = true
		} else {
			td = &rtTurnDetection{}
			if err := json.Unmarshal(*in.TurnDetection, td); err != nil || td.Type != "server_vad" {
				rt.sendError("unsupported_turn_detection", `turn_detection must be {"type":"server_vad"} or null`, clientEvent)
				return
			}
		}
	}
	rt.mu.Lock()
	if model != nil {
		rt.model = model
	}
	if in.Transcription != nil {
		rt.language = strings.ToLower(in.Transcription.Language)
		rt.prompt = in.Transcription.Prompt
	}
	switch {
	case turnOff:
		rt.detect = false
	case td != nil:
		rt.detect = true
		if td.Threshold != nil && *td.Threshold > 0 && *td.Threshold < 1 {
			rt.thresh = *td.Threshold
		}
		if td.PrefixPaddingMS != nil && *td.PrefixPaddingMS >= 0 {
			rt.prefixMS = *td.PrefixPaddingMS
		}
		if td.SilenceDurationMS != nil && *td.SilenceDurationMS >= 100 {
			rt.silentMS = *td.SilenceDurationMS
		}
	}
	rt.mu.Unlock()
	rt.send(map[string]any{"type": "session.updated", "session": rt.sessionObject()})
}

// spans runs voice activity detection over samples at rtRate.
func (rt *realtime) spans(samples []float32, thresh float64, silentMS int) ([]audio.Interval, error) {
	if rt.vadModel == nil {
		return audio.EnergySpans(samples, rtRate, float64(silentMS)/1000, 0.15), nil
	}
	got, err := rt.s.detect(rt.ctx, rt.vadModel, samples, rtRate, runner.DetectOptions{Threshold: thresh, MinSilenceMS: silentMS, MinSpeechMS: 150})
	if err != nil {
		return nil, err
	}
	out := make([]audio.Interval, len(got))
	for i, g := range got {
		out[i] = audio.Interval{Start: g.Start, End: g.End}
	}
	return out, nil
}

// detectLoop looks at the uncommitted audio a few times a second and decides
// where utterances start and end.
func (rt *realtime) detectLoop() {
	ticker := time.NewTicker(rtTick)
	defer ticker.Stop()
	for {
		select {
		case <-rt.ctx.Done():
			return
		case <-ticker.C:
		}
		rt.mu.Lock()
		if !rt.detect || !rt.dirty || len(rt.buf) == 0 {
			rt.mu.Unlock()
			continue
		}
		rt.dirty = false
		snapshot := rt.buf // appends never modify existing samples
		start := rt.bufStart
		thresh, silentMS, prefixMS := rt.thresh, rt.silentMS, rt.prefixMS
		rt.mu.Unlock()

		spans, err := rt.spans(snapshot, thresh, silentMS)
		if err != nil {
			if rt.ctx.Err() == nil {
				rt.s.log.Error("realtime voice activity detection failed", "err", err)
			}
			continue
		}
		length := float64(len(snapshot)) / rtRate
		ms := func(sec float64) int { return int((float64(start)/rtRate + sec) * 1000) }
		silence := float64(silentMS) / 1000

		consumed := 0.0 // seconds of the snapshot that are finished with
		for i, sp := range spans {
			ended := length-sp.End >= silence
			tooLong := length-sp.Start >= rtMaxTurn
			last := i == len(spans)-1

			rt.mu.Lock()
			if !rt.speaking {
				rt.speaking, rt.itemID = true, newID("item")
				item := rt.itemID
				rt.mu.Unlock()
				rt.send(map[string]any{"type": "input_audio_buffer.speech_started", "item_id": item,
					"audio_start_ms": ms(maxf(sp.Start-float64(prefixMS)/1000, 0))})
			} else {
				rt.mu.Unlock()
			}
			if !ended && !tooLong && last {
				break // still talking
			}
			end := sp.End
			if tooLong && !ended {
				end = length
			}
			from := int(maxf(sp.Start-float64(prefixMS)/1000, consumed) * rtRate)
			to := int((end + 0.2) * rtRate) // a little trailing audio helps the recognizer
			if to > len(snapshot) {
				to = len(snapshot)
			}
			rt.mu.Lock()
			item := rt.itemID
			rt.speaking, rt.itemID = false, ""
			rt.mu.Unlock()
			rt.send(map[string]any{"type": "input_audio_buffer.speech_stopped", "item_id": item, "audio_end_ms": ms(end)})
			rt.commit(rtUtterance{itemID: item, samples: append([]float32(nil), snapshot[from:to]...)})
			consumed = float64(to) / rtRate
		}
		// Drop what has been committed, and old silence when nobody is speaking.
		rt.mu.Lock()
		drop := int(consumed * rtRate)
		if !rt.speaking {
			if keep := len(snapshot) - int(rtKeepSilence*rtRate); keep > drop {
				drop = keep
			}
		}
		if drop > len(rt.buf) {
			drop = len(rt.buf)
		}
		if drop > 0 {
			rt.buf = append([]float32(nil), rt.buf[drop:]...)
			rt.bufStart += drop
		}
		rt.mu.Unlock()
	}
}

// commit announces an utterance and queues it for transcription.
func (rt *realtime) commit(u rtUtterance) {
	rt.mu.Lock()
	prev := rt.lastItem
	rt.lastItem = u.itemID
	rt.mu.Unlock()
	committed := map[string]any{"type": "input_audio_buffer.committed", "item_id": u.itemID}
	added := map[string]any{"type": "conversation.item.added", "item": map[string]any{
		"id": u.itemID, "object": "realtime.item", "type": "message", "role": "user", "status": "completed",
		"content": []map[string]any{{"type": "input_audio", "transcript": nil}},
	}}
	if prev != "" {
		committed["previous_item_id"] = prev
		added["previous_item_id"] = prev
	}
	rt.send(committed)
	rt.send(added)
	select {
	case rt.queue <- u:
	case <-rt.ctx.Done():
	}
}

// transcribeLoop transcribes utterances one at a time, in order.
func (rt *realtime) transcribeLoop() {
	for {
		var u rtUtterance
		select {
		case <-rt.ctx.Done():
			return
		case u = <-rt.queue:
		}
		rt.mu.Lock()
		model, language, prompt := rt.model, rt.language, rt.prompt
		rt.mu.Unlock()
		fail := func(err error) {
			rt.send(map[string]any{"type": "conversation.item.input_audio_transcription.failed", "item_id": u.itemID, "content_index": 0,
				"error": map[string]any{"type": "transcription_error", "message": err.Error()}})
		}
		if model == nil {
			fail(errors.New("no transcription model: pass ?model= when connecting or set audio.input.transcription.model in session.update"))
			continue
		}
		opts := runner.TranscribeOptions{SampleRate: model.Audio.SampleRate, Language: language}
		if model.Has(manifest.CapPrompt) {
			opts.Prompt = prompt
		}
		pcm := audio.FloatToS16(audio.Resample(u.samples, rtRate, model.Audio.SampleRate))
		lease, err := rt.s.sched.Acquire(rt.ctx, model, nil)
		if err != nil {
			fail(err)
			continue
		}
		text := ""
		_, err = lease.Conn.Transcribe(rt.ctx, pcm, opts, func(seg runner.Segment) error {
			prev := text
			text = JoinText(text, seg.Text)
			rt.send(map[string]any{"type": "conversation.item.input_audio_transcription.delta", "item_id": u.itemID,
				"content_index": 0, "delta": text[len(prev):]})
			return nil
		})
		lease.Release()
		if err != nil {
			if rt.ctx.Err() == nil {
				fail(err)
			}
			continue
		}
		seconds := float64(len(u.samples)) / rtRate
		rt.send(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": u.itemID,
			"content_index": 0, "transcript": text, "usage": map[string]any{"type": "duration", "seconds": seconds}})
	}
}

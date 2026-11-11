package server

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/eastsidegunn/oaudio/internal/audio"
	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/runner"
)

// Streaming recognition in realtime sessions (ADR 0008).
//
// With a model that declares the "stream" capability and server-side turn
// detection on, an utterance is sent to the runner while it is being spoken,
// a piece every detection tick, and each change in its transcript reaches the
// client at once as conversation.item.input_audio_transcription.delta. When
// the turn ends, the runner finishes the stream and the final transcript
// arrives as .completed. Other models keep the original path: the whole
// utterance is transcribed after it ends.

type rtOpKind int

const (
	rtFeed  rtOpKind = iota // more audio of the utterance
	rtEnd                   // the utterance is over
	rtAbort                 // the utterance was dropped (input_audio_buffer.clear)
)

type rtOp struct {
	kind    rtOpKind
	item    string
	samples []float32 // at rtRate, for rtFeed
}

// rtStream is one utterance being recognized as it is spoken.
type rtStream struct {
	item, session string
	model         *manifest.Manifest
	language      string
	in            []float32 // the utterance so far, at rtRate
	sent          int       // samples at the model's rate already sent
	started       bool      // the runner knows the stream
	emitted       string    // transcript already sent as deltas
	failed        bool
}

func (rt *realtime) enqueue(op rtOp) {
	select {
	case rt.ops <- op:
	case <-rt.ctx.Done():
	}
}

// feed queues the audio of the streamed utterance between what was already
// queued and absolute sample start+upto of snapshot.
func (rt *realtime) feed(snapshot []float32, start, upto int) {
	rt.mu.Lock()
	item := rt.streamItem
	from := rt.streamPos - start
	if item == "" || upto <= from {
		rt.mu.Unlock()
		return
	}
	if from < 0 {
		from = 0
	}
	rt.streamPos = start + upto
	rt.mu.Unlock()
	rt.enqueue(rtOp{kind: rtFeed, item: item, samples: append([]float32(nil), snapshot[from:upto]...)})
}

// streamLoop sends queued audio to the runner, one request at a time, and
// turns the transcripts that come back into events.
func (rt *realtime) streamLoop() {
	var cur *rtStream
	defer func() {
		if cur != nil && cur.started && !cur.failed {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			rt.closeStream(ctx, cur)
			cancel()
		}
	}()
	for {
		var op rtOp
		select {
		case <-rt.ctx.Done():
			return
		case op = <-rt.ops:
		}
		if op.kind == rtAbort {
			if cur != nil && cur.item == op.item {
				if cur.started && !cur.failed {
					rt.closeStream(rt.ctx, cur)
				}
				cur = nil
			}
			continue
		}
		if cur == nil || cur.item != op.item {
			if cur != nil && cur.started && !cur.failed {
				rt.closeStream(rt.ctx, cur)
			}
			rt.mu.Lock()
			cur = &rtStream{item: op.item, session: newID("stream"), model: rt.model, language: rt.language}
			rt.mu.Unlock()
		}
		rate := cur.model.Audio.SampleRate
		switch op.kind {
		case rtFeed:
			cur.in = append(cur.in, op.samples...)
			rt.streamStep(cur, audio.ResampleStable(len(cur.in), rtRate, rate), false)
		case rtEnd:
			total := len(cur.in)
			if rate != rtRate {
				total = int(math.Floor(float64(len(cur.in)) * float64(rate) / rtRate))
			}
			rt.streamStep(cur, total, true)
			cur = nil
		}
	}
}

// streamStep sends the samples of cur up to upto (at the model's rate) and
// reports what the transcript gained.
func (rt *realtime) streamStep(cur *rtStream, upto int, end bool) {
	if cur.failed || (!end && upto <= cur.sent) {
		return
	}
	rate := cur.model.Audio.SampleRate
	pcm := audio.FloatToS16(audio.ResampleRange(cur.in, rtRate, rate, cur.sent, upto))
	opts := runner.StreamOptions{Session: cur.session, SampleRate: rate, Language: cur.language, Start: !cur.started, End: end}
	lease, err := rt.s.sched.Acquire(rt.ctx, cur.model, nil)
	if err != nil {
		rt.streamFailed(cur, err)
		return
	}
	res, err := lease.Conn.Stream(rt.ctx, opts, pcm)
	lease.Release()
	if err != nil {
		rt.streamFailed(cur, err)
		return
	}
	cur.started, cur.sent = true, upto
	// Deltas only ever add to the end. Should a model revise words it has
	// already reported, nothing is sent until its transcript grows past them
	// again; .completed always carries the final transcript in full.
	if strings.HasPrefix(res.Text, cur.emitted) && len(res.Text) > len(cur.emitted) {
		rt.send(map[string]any{"type": "conversation.item.input_audio_transcription.delta", "item_id": cur.item,
			"content_index": 0, "delta": res.Text[len(cur.emitted):]})
		cur.emitted = res.Text
	}
	if end {
		rt.send(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": cur.item,
			"content_index": 0, "transcript": res.Text,
			"usage": map[string]any{"type": "duration", "seconds": float64(len(cur.in)) / rtRate}})
	}
}

func (rt *realtime) streamFailed(cur *rtStream, err error) {
	if rt.ctx.Err() != nil {
		return
	}
	cur.failed = true
	rt.send(map[string]any{"type": "conversation.item.input_audio_transcription.failed", "item_id": cur.item, "content_index": 0,
		"error": map[string]any{"type": "transcription_error", "message": err.Error()}})
	if cur.started {
		rt.closeStream(rt.ctx, cur)
	}
}

func (rt *realtime) closeStream(ctx context.Context, cur *rtStream) {
	lease, err := rt.s.sched.Acquire(ctx, cur.model, nil)
	if err != nil {
		return
	}
	lease.Conn.CloseStream(ctx, cur.session)
	lease.Release()
}

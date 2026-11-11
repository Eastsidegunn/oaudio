package runner_test

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eastsidegunn/oaudio/internal/engines/ref"
	"github.com/eastsidegunn/oaudio/runner"
)

// tones returns n tone bursts of the reference engine's shape as PCM: 100 ms
// of tone and 60 ms of silence each.
func tones(n, rate int) []byte {
	var pcm []byte
	for w := 0; w < n; w++ {
		for i := 0; i < rate*160/1000; i++ {
			var v int16
			if i < rate/10 {
				v = int16(0.5 * 32767 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
			}
			pcm = append(pcm, byte(v), byte(v>>8))
		}
	}
	return pcm
}

func dialRef(t *testing.T, engine runner.Engine) *runner.Conn {
	t.Helper()
	ts := httptest.NewServer(runner.Handler(engine))
	t.Cleanup(ts.Close)
	c, err := runner.Dial(context.Background(), strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A stream is started, grows, and ends; the answers carry the whole
// transcript so far, and an ended stream is forgotten.
func TestStreamLifecycle(t *testing.T) {
	engine, _ := ref.New(runner.Model{})
	c := dialRef(t, engine)
	ctx := context.Background()
	const rate = 16000
	audio := tones(3, rate)
	cut := len(audio) / 3 * 2 // two words and a bit
	opts := runner.StreamOptions{Session: "s1", SampleRate: rate, Start: true}
	res, err := c.Stream(ctx, opts, audio[:cut])
	if err != nil || res.Done || res.Text != "word1 word2" {
		t.Fatalf("first piece: %+v %v", res, err)
	}
	opts.Start = false
	res, err = c.Stream(ctx, opts, audio[cut:cut+rate/20*2]) // the third tone, still sounding
	if err != nil || res.Text != "word1 word2" {
		t.Fatalf("mid-word: %+v %v", res, err)
	}
	opts.End = true
	res, err = c.Stream(ctx, opts, audio[cut+rate/20*2:])
	if err != nil || !res.Done || res.Text != "word1 word2 word3" || res.Language != "en" {
		t.Fatalf("end: %+v %v", res, err)
	}
	// Ended: the session is unknown now, and only a start brings it back.
	opts.End = false
	var re *runner.Error
	if _, err := c.Stream(ctx, opts, audio[:320]); !errors.As(err, &re) || re.Status != 404 {
		t.Errorf("after end: %v, want 404", err)
	}
	// An abandoned stream is gone too.
	c.Stream(ctx, runner.StreamOptions{Session: "s2", SampleRate: rate, Start: true}, audio[:320])
	if err := c.CloseStream(ctx, "s2"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stream(ctx, runner.StreamOptions{Session: "s2", SampleRate: rate}, audio[:320]); !errors.As(err, &re) || re.Status != 404 {
		t.Errorf("after close: %v, want 404", err)
	}
}

// Refusals reach the daemon as statuses: a bad request as 400, a runner that
// does not stream as 501.
func TestStreamRefusals(t *testing.T) {
	engine, _ := ref.New(runner.Model{})
	c := dialRef(t, engine)
	ctx := context.Background()
	var re *runner.Error
	if _, err := c.Stream(ctx, runner.StreamOptions{Session: "x", SampleRate: 16000, Language: "xx", Start: true}, nil); !errors.As(err, &re) || re.Status != 400 {
		t.Errorf("unsupported language: %v, want 400", err)
	}
	plain := dialRef(t, refusing{})
	if _, err := plain.Stream(ctx, runner.StreamOptions{Session: "x", SampleRate: 16000, Start: true}, nil); !errors.As(err, &re) || re.Status != 501 {
		t.Errorf("not a streamer: %v, want 501", err)
	}
}

package runner_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eastsidegunn/oaudio/runner"
)

// refusing is a transcriber that fails in a chosen way.
type refusing struct {
	before error // returned before any segment
	after  error // returned after one segment
}

func (refusing) Info() runner.Info { return runner.Info{Engine: "test", Tasks: []string{"stt"}} }
func (refusing) Close() error      { return nil }
func (r refusing) Transcribe(_ context.Context, _ []float32, _ runner.TranscribeOptions, emit func(runner.Segment) error) (string, error) {
	if r.before != nil {
		return "", r.before
	}
	emit(runner.Segment{End: 1, Text: "one"})
	return "", r.after
}

// An engine that refuses a recognition before producing anything gets the
// same statuses as the other endpoints; one that fails midway can only end
// the stream with an error event, which the daemon reports as a failure.
func TestTranscribeErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		engine refusing
		status int
		segs   int
	}{
		{"refused", refusing{before: runner.BadRequest("language %q is not supported", "xx")}, 400, 0},
		{"failed", refusing{before: errors.New("engine crashed")}, 500, 0},
		{"failed midway", refusing{after: runner.BadRequest("too late to refuse")}, 500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(runner.Handler(tc.engine))
			defer ts.Close()
			c, err := runner.Dial(context.Background(), strings.TrimPrefix(ts.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			segs := 0
			_, err = c.Transcribe(context.Background(), make([]byte, 320), runner.TranscribeOptions{SampleRate: 16000},
				func(runner.Segment) error { segs++; return nil })
			var re *runner.Error
			if !errors.As(err, &re) || re.Status != tc.status || segs != tc.segs {
				t.Errorf("err %#v after %d segments, want status %d after %d", err, segs, tc.status, tc.segs)
			}
		})
	}
}

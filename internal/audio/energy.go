package audio

import (
	"math"
	"sort"
)

// Interval is a stretch of audio in seconds.
type Interval struct{ Start, End float64 }

// EnergySpans finds speech by loudness alone: frames well above the
// recording's own noise floor, joined across gaps shorter than minSilence.
//
// It is a fallback for when no voice activity detection model is installed.
// It cannot tell speech from other loud sounds, so a real model is better
// whenever one is available.
func EnergySpans(samples []float32, rate int, minSilence, minSpeech float64) []Interval {
	frame := rate / 50 // 20 ms
	n := len(samples) / frame
	if frame == 0 || n == 0 {
		return nil
	}
	db := make([]float64, n)
	for i := range db {
		var sum float64
		for _, s := range samples[i*frame : (i+1)*frame] {
			sum += float64(s) * float64(s)
		}
		db[i] = 10 * math.Log10(sum/float64(frame)+1e-12)
	}
	sorted := append([]float64(nil), db...)
	sort.Float64s(sorted)
	floor := sorted[n/10]
	// Speech must be 12 dB above the quietest tenth of the recording, and
	// never quieter than -50 dBFS, so digital silence does not count.
	threshold := math.Max(floor+12, -50)

	var spans []Interval
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		s, e := float64(start*frame)/float64(rate), float64(end*frame)/float64(rate)
		if len(spans) > 0 && s-spans[len(spans)-1].End < minSilence {
			spans[len(spans)-1].End = e
		} else {
			spans = append(spans, Interval{s, e})
		}
		start = -1
	}
	for i, v := range db {
		switch {
		case v > threshold && start < 0:
			start = i
		case v <= threshold && start >= 0:
			flush(i)
		}
	}
	flush(n)
	out := spans[:0]
	for _, sp := range spans {
		if sp.End-sp.Start >= minSpeech {
			out = append(out, sp)
		}
	}
	return out
}

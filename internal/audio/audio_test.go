package audio

import (
	"math"
	"strings"
	"testing"
)

func sine(freq float64, rate, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(0.5 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
	}
	return out
}

func rms(x []float32) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

func TestWAVRoundTrip(t *testing.T) {
	in := sine(440, 16000, 1600)
	got, rate, err := DecodeWAV(EncodeWAV(FloatToS16(in), 16000))
	if err != nil || rate != 16000 || len(got) != len(in) {
		t.Fatalf("decode: err=%v rate=%d len=%d", err, rate, len(got))
	}
	for i := range in {
		if math.Abs(float64(in[i]-got[i])) > 1.0/32768+1e-6 {
			t.Fatalf("sample %d: %v != %v", i, in[i], got[i])
		}
	}
}

func TestDecodeStreamedWAV(t *testing.T) {
	pcm := FloatToS16(sine(440, 22050, 2205))
	data := append(WAVHeader(22050, -1), pcm...) // length unknown, as in a streamed response
	got, rate, err := DecodeWAV(data)
	if err != nil || rate != 22050 || len(got) != 2205 {
		t.Fatalf("err=%v rate=%d len=%d", err, rate, len(got))
	}
}

func TestDecodeRejectsNonWAV(t *testing.T) {
	if _, _, err := DecodeWAV([]byte("ID3 not a wav file at all")); err != ErrNotWAV {
		t.Fatalf("got %v, want ErrNotWAV", err)
	}
}

// A tone below the new Nyquist frequency must survive resampling with its
// level and pitch intact; a tone above it must be removed, not aliased.
func TestResample(t *testing.T) {
	out := Resample(sine(1000, 48000, 48000), 48000, 16000)
	if len(out) != 16000 {
		t.Fatalf("length %d, want 16000", len(out))
	}
	mid := out[2000:14000]
	if r := rms(mid); math.Abs(r-0.5/math.Sqrt2) > 0.005 {
		t.Errorf("1 kHz tone level changed: rms %.4f", r)
	}
	crossings := 0
	for i := 1; i < len(mid); i++ {
		if (mid[i-1] < 0) != (mid[i] < 0) {
			crossings++
		}
	}
	if freq := float64(crossings) / 2 / (float64(len(mid)) / 16000); math.Abs(freq-1000) > 5 {
		t.Errorf("1 kHz tone came out at %.1f Hz", freq)
	}
	alias := Resample(sine(11000, 48000, 48000), 48000, 16000)
	if r := rms(alias[2000:14000]); r > 0.005 {
		t.Errorf("11 kHz tone aliased into the 16 kHz output: rms %.4f", r)
	}
	up := Resample(sine(1000, 16000, 16000), 16000, 24000)
	if r := rms(up[2000:22000]); math.Abs(r-0.5/math.Sqrt2) > 0.005 {
		t.Errorf("upsampled level changed: rms %.4f", r)
	}
}

func TestSubtitles(t *testing.T) {
	cues := []Cue{{0, 2.46, " Hello."}, {3661.5, 3662, "Bye."}}
	srt := SRT(cues)
	if !strings.Contains(srt, "1\n00:00:00,000 --> 00:00:02,460\nHello.\n\n2\n01:01:01,500 --> 01:01:02,000\nBye.\n") {
		t.Errorf("bad SRT:\n%s", srt)
	}
	vtt := VTT(cues)
	if !strings.HasPrefix(vtt, "WEBVTT\n\n00:00:00.000 --> 00:00:02.460\nHello.\n") {
		t.Errorf("bad VTT:\n%s", vtt)
	}
}

package server

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/eastsidegunn/oaudio/internal/audio"
)

// chunky returns the bytes in reads of random length, odd ones included.
type chunky struct {
	data []byte
	rng  *rand.Rand
}

func (c *chunky) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := 1 + c.rng.Intn(3000)
	if n > len(c.data) {
		n = len(c.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func tone(rate int, seconds float64) []byte {
	s := make([]float32, int(float64(rate)*seconds))
	for i := range s {
		t := float64(i) / float64(rate)
		s[i] = float32(0.4*math.Sin(2*math.Pi*220*t) + 0.2*math.Sin(2*math.Pi*3100*t))
	}
	return audio.FloatToS16(s)
}

// Resampling while streaming gives the same samples as resampling the whole.
func TestCopyPCMResampledMatchesWhole(t *testing.T) {
	for _, rates := range [][2]int{{48000, 24000}, {44100, 24000}, {16000, 24000}, {22050, 24000}} {
		in := tone(rates[0], 1.3)
		want := audio.FloatToS16(audio.Resample(audio.S16ToFloat(in), rates[0], rates[1]))
		for seed := int64(0); seed < 3; seed++ {
			var got bytes.Buffer
			if _, err := copyPCM(&got, &chunky{data: in, rng: rand.New(rand.NewSource(seed))}, rates[0], rates[1], func() {}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("%d -> %d Hz, seed %d: %d bytes differ from resampling the whole (%d bytes)", rates[0], rates[1], seed, got.Len(), len(want))
			}
		}
	}
}

// Resampled output starts before the input has ended.
func TestCopyPCMResampledStreams(t *testing.T) {
	pr, pw := io.Pipe()
	first := make(chan struct{})
	done := make(chan struct{})
	var once bool
	go func() {
		defer close(done)
		copyPCM(io.Discard, pr, 48000, 24000, func() {
			if !once {
				once = true
				close(first)
			}
		})
	}()
	pw.Write(tone(48000, 0.5))
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("no output before the input ended")
	}
	pw.Close()
	<-done
}

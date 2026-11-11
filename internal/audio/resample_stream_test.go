package audio

import (
	"math"
	"math/rand"
	"testing"
)

// Resampling a stream piece by piece, sending only the stable samples and the
// rest at the end, must give exactly what resampling it whole gives.
func TestResampleInPieces(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	in := make([]float32, 24000*3+123)
	for i := range in {
		in[i] = float32(0.3*math.Sin(float64(i)*0.05) + 0.1*rng.Float64())
	}
	for _, rates := range [][2]int{{24000, 16000}, {24000, 24000}, {16000, 24000}, {24000, 22050}} {
		from, to := rates[0], rates[1]
		whole := Resample(in, from, to)
		var got []float32
		sent := 0
		for n := 0; n < len(in); {
			n += 1000 + rng.Intn(5000)
			if n > len(in) {
				n = len(in)
			}
			stable := ResampleStable(n, from, to)
			if stable > sent {
				got = append(got, ResampleRange(in[:n], from, to, sent, stable)...)
				sent = stable
			}
		}
		total := len(whole)
		got = append(got, ResampleRange(in, from, to, sent, total)...)
		if len(got) != len(whole) {
			t.Fatalf("%d->%d: %d samples, want %d", from, to, len(got), len(whole))
		}
		for i := range whole {
			if got[i] != whole[i] {
				t.Fatalf("%d->%d: sample %d differs: %v vs %v", from, to, i, got[i], whole[i])
			}
		}
	}
}

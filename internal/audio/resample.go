package audio

import "math"

// sincHalfWidth is the number of zero crossings on each side of the kernel.
const sincHalfWidth = 16

// Resample converts mono samples from one rate to another with a Hann-windowed
// sinc filter. When downsampling, the cutoff drops to the new Nyquist
// frequency so content above it does not alias into the result.
func Resample(in []float32, from, to int) []float32 {
	if from == to || len(in) == 0 || from <= 0 || to <= 0 {
		return in
	}
	ratio := float64(to) / float64(from)
	cutoff := 1.0
	if ratio < 1 {
		cutoff = ratio
	}
	cutoff *= 0.97 // leave a small transition band below Nyquist
	half := float64(sincHalfWidth) / cutoff
	n := int(math.Floor(float64(len(in)) * ratio))
	out := make([]float32, n)
	for i := range out {
		center := float64(i) / ratio
		lo := int(math.Ceil(center - half))
		hi := int(math.Floor(center + half))
		if lo < 0 {
			lo = 0
		}
		if hi > len(in)-1 {
			hi = len(in) - 1
		}
		var acc, norm float64
		for k := lo; k <= hi; k++ {
			x := center - float64(k)
			w := 0.5 * (1 + math.Cos(math.Pi*x/half)) // Hann window
			h := w * sinc(x*cutoff)
			acc += float64(in[k]) * h
			norm += h
		}
		if norm != 0 {
			acc /= norm // unity gain at DC, including at the edges
		}
		out[i] = float32(acc)
	}
	return out
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

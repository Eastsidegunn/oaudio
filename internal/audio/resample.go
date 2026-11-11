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
	return ResampleRange(in, from, to, 0, int(math.Floor(float64(len(in))*float64(to)/float64(from))))
}

// ResampleRange returns samples [i0, i1) of what Resample(in, from, to) would
// return, computing only those.
func ResampleRange(in []float32, from, to, i0, i1 int) []float32 {
	if from == to || from <= 0 || to <= 0 {
		return append([]float32(nil), in[i0:i1]...)
	}
	ratio, half, cutoff := kernel(from, to)
	out := make([]float32, 0, i1-i0)
	for i := i0; i < i1; i++ {
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
		out = append(out, float32(acc))
	}
	return out
}

// ResampleStable returns how many leading samples of Resample(in, from, to),
// for an in of length n, would stay the same if more input were appended:
// those whose filter kernel lies wholly inside the input so far. A stream
// resampled piece by piece sends these and holds back the rest until more
// input, or the end, arrives.
func ResampleStable(n, from, to int) int {
	if from == to || from <= 0 || to <= 0 {
		return n
	}
	ratio, half, _ := kernel(from, to)
	total := int(math.Floor(float64(n) * ratio))
	// Sample i needs input up to floor(i/ratio + half), which must be < n.
	stable := int(math.Ceil((float64(n-1)-half)*ratio)) - 1
	for stable+1 < total && math.Floor(float64(stable+1)/ratio+half) <= float64(n-1) {
		stable++
	}
	for stable >= 0 && math.Floor(float64(stable)/ratio+half) > float64(n-1) {
		stable--
	}
	if stable+1 < 0 {
		return 0
	}
	if stable+1 > total {
		return total
	}
	return stable + 1
}

func kernel(from, to int) (ratio, half, cutoff float64) {
	ratio = float64(to) / float64(from)
	cutoff = 1.0
	if ratio < 1 {
		cutoff = ratio
	}
	cutoff *= 0.97 // leave a small transition band below Nyquist
	return ratio, float64(sincHalfWidth) / cutoff, cutoff
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

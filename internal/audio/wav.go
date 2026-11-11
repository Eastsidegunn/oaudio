// Package audio holds the PCM plumbing the daemon does on behalf of every
// engine: decoding uploads, resampling, and encoding responses. Runners only
// ever see mono 16-bit PCM.
package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrNotWAV is returned when the input is not a RIFF/WAVE file.
var ErrNotWAV = errors.New("not a WAV file")

// DecodeWAV parses a WAV file into mono float32 samples in [-1, 1].
// Multi-channel audio is averaged to mono. PCM 8/16/24/32-bit and IEEE float
// 32/64-bit are supported, including WAVE_FORMAT_EXTENSIBLE and streamed files
// whose data chunk has no length.
func DecodeWAV(data []byte) (samples []float32, sampleRate int, err error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, ErrNotWAV
	}
	var (
		format, channels, bits int
		haveFmt                bool
	)
	pos := 12
	for pos+8 <= len(data) {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(data) {
				return nil, 0, errors.New("wav: truncated fmt chunk")
			}
			format = int(binary.LittleEndian.Uint16(data[body:]))
			channels = int(binary.LittleEndian.Uint16(data[body+2:]))
			sampleRate = int(binary.LittleEndian.Uint32(data[body+4:]))
			bits = int(binary.LittleEndian.Uint16(data[body+14:]))
			if format == 0xFFFE && size >= 26 && body+26 <= len(data) {
				format = int(binary.LittleEndian.Uint16(data[body+24:])) // sub-format
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, 0, errors.New("wav: data chunk before fmt chunk")
			}
			end := body + size
			// Streamed WAVs write 0 or 0xFFFFFFFF as the length: read to EOF.
			if size == 0 || size == 0xFFFFFFFF || end > len(data) || end < body {
				end = len(data)
			}
			samples, err = pcmToMono(data[body:end], format, channels, bits)
			return samples, sampleRate, err
		}
		pos = body + size + size&1
		if pos < body { // overflow
			break
		}
	}
	return nil, 0, errors.New("wav: no data chunk")
}

func pcmToMono(raw []byte, format, channels, bits int) ([]float32, error) {
	if channels < 1 || channels > 64 {
		return nil, fmt.Errorf("wav: unsupported channel count %d", channels)
	}
	bps := bits / 8
	var conv func(b []byte) float64
	switch {
	case format == 1 && bits == 8:
		conv = func(b []byte) float64 { return (float64(b[0]) - 128) / 128 }
	case format == 1 && bits == 16:
		conv = func(b []byte) float64 { return float64(int16(binary.LittleEndian.Uint16(b))) / 32768 }
	case format == 1 && bits == 24:
		conv = func(b []byte) float64 {
			v := int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
			return float64(v) / 8388608
		}
	case format == 1 && bits == 32:
		conv = func(b []byte) float64 { return float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648 }
	case format == 3 && bits == 32:
		conv = func(b []byte) float64 { return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))) }
	case format == 3 && bits == 64:
		conv = func(b []byte) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b)) }
	default:
		return nil, fmt.Errorf("wav: unsupported encoding (format tag %d, %d bits)", format, bits)
	}
	frame := bps * channels
	n := len(raw) / frame
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		var sum float64
		for c := 0; c < channels; c++ {
			off := i*frame + c*bps
			sum += conv(raw[off : off+bps])
		}
		out[i] = float32(sum / float64(channels))
	}
	return out, nil
}

// FloatToS16 converts float samples to little-endian 16-bit PCM bytes.
func FloatToS16(in []float32) []byte {
	out := make([]byte, len(in)*2)
	for i, s := range in {
		v := float64(s) * 32768
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(math.Round(v))))
	}
	return out
}

// S16ToFloat converts little-endian 16-bit PCM bytes to float samples.
func S16ToFloat(in []byte) []float32 {
	out := make([]float32, len(in)/2)
	for i := range out {
		out[i] = float32(int16(binary.LittleEndian.Uint16(in[i*2:]))) / 32768
	}
	return out
}

// WAVHeader returns a 44-byte header for mono 16-bit PCM. Pass dataBytes < 0
// for a stream of unknown length; players read such files until EOF.
func WAVHeader(sampleRate int, dataBytes int64) []byte {
	var riff, data uint32 = 0xFFFFFFFF, 0xFFFFFFFF
	if dataBytes >= 0 && dataBytes <= math.MaxUint32-36 {
		riff, data = uint32(dataBytes+36), uint32(dataBytes)
	}
	b := new(bytes.Buffer)
	b.WriteString("RIFF")
	binary.Write(b, binary.LittleEndian, riff)
	b.WriteString("WAVEfmt ")
	binary.Write(b, binary.LittleEndian, uint32(16))
	binary.Write(b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(b, binary.LittleEndian, uint16(1)) // mono
	binary.Write(b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(b, binary.LittleEndian, uint32(sampleRate*2)) // byte rate
	binary.Write(b, binary.LittleEndian, uint16(2))            // block align
	binary.Write(b, binary.LittleEndian, uint16(16))           // bits
	b.WriteString("data")
	binary.Write(b, binary.LittleEndian, data)
	return b.Bytes()
}

// EncodeWAV wraps 16-bit mono PCM in a WAV container.
func EncodeWAV(pcm []byte, sampleRate int) []byte {
	return append(WAVHeader(sampleRate, int64(len(pcm))), pcm...)
}

// WriteWAV writes a complete WAV file.
func WriteWAV(w io.Writer, pcm []byte, sampleRate int) error {
	_, err := w.Write(EncodeWAV(pcm, sampleRate))
	return err
}

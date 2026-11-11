package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

// WAV and raw PCM are handled natively. Every other container or codec is
// delegated to an ffmpeg binary if one is installed; Oaudio never links it.

var (
	ffmpegOnce sync.Once
	ffmpegPath string
)

// FFmpeg returns the path of the ffmpeg binary, or "" if none is available.
// Set OAUDIO_FFMPEG to a path to choose one, or to "off" to disable it.
func FFmpeg() string {
	ffmpegOnce.Do(func() {
		switch v := os.Getenv("OAUDIO_FFMPEG"); v {
		case "off":
		case "":
			ffmpegPath, _ = exec.LookPath("ffmpeg")
		default:
			ffmpegPath = v
		}
	})
	return ffmpegPath
}

// ErrNeedFFmpeg explains what is missing when a format needs ffmpeg.
var ErrNeedFFmpeg = errors.New("this audio format needs ffmpeg, which was not found on PATH; install ffmpeg or use WAV")

// Decode turns an uploaded audio file into mono float samples at rate.
// WAV is decoded natively; anything else goes through ffmpeg.
func Decode(ctx context.Context, data []byte, rate int) ([]float32, error) {
	samples, srcRate, err := DecodeWAV(data)
	if err == nil {
		return Resample(samples, srcRate, rate), nil
	}
	if !errors.Is(err, ErrNotWAV) && FFmpeg() == "" {
		return nil, err
	}
	if FFmpeg() == "" {
		return nil, ErrNeedFFmpeg
	}
	// Some containers (mp4/m4a) need a seekable input, so use a temp file.
	tmp, terr := os.CreateTemp("", "oaudio-in-*")
	if terr != nil {
		return nil, terr
	}
	defer os.Remove(tmp.Name())
	if _, werr := tmp.Write(data); werr != nil {
		tmp.Close()
		return nil, werr
	}
	tmp.Close()
	cmd := exec.CommandContext(ctx, FFmpeg(), "-nostdin", "-v", "error", "-i", tmp.Name(),
		"-vn", "-ac", "1", "-ar", strconv.Itoa(rate), "-f", "s16le", "pipe:1")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if rerr := cmd.Run(); rerr != nil {
		return nil, fmt.Errorf("could not decode audio: %s", firstLine(stderr.String(), rerr))
	}
	return S16ToFloat(out.Bytes()), nil
}

// Format describes a response encoding.
type Format struct {
	Name        string
	ContentType string
	ffmpegArgs  []string // nil for native formats
}

var formats = map[string]Format{
	"wav":  {Name: "wav", ContentType: "audio/wav"},
	"pcm":  {Name: "pcm", ContentType: "audio/pcm"},
	"mp3":  {Name: "mp3", ContentType: "audio/mpeg", ffmpegArgs: []string{"-c:a", "libmp3lame", "-b:a", "128k", "-f", "mp3"}},
	"opus": {Name: "opus", ContentType: "audio/ogg", ffmpegArgs: []string{"-c:a", "libopus", "-b:a", "48k", "-f", "ogg"}},
	"aac":  {Name: "aac", ContentType: "audio/aac", ffmpegArgs: []string{"-c:a", "aac", "-b:a", "128k", "-f", "adts"}},
	"flac": {Name: "flac", ContentType: "audio/flac", ffmpegArgs: []string{"-c:a", "flac", "-f", "flac"}},
}

// LookupFormat returns the named response format.
func LookupFormat(name string) (Format, bool) {
	f, ok := formats[name]
	return f, ok
}

// Native reports whether the format needs no external encoder.
func (f Format) Native() bool { return f.ffmpegArgs == nil }

// Encoder accepts mono 16-bit PCM and writes the encoded stream to w as it
// goes, so responses can start before synthesis has finished.
type Encoder interface {
	io.WriteCloser
}

// NewEncoder starts an encoder for format f. The PCM content type carries the
// sample rate in a parameter, since raw PCM has no header to say it.
func NewEncoder(ctx context.Context, f Format, sampleRate int, w io.Writer) (Encoder, error) {
	switch f.Name {
	case "pcm":
		return nopCloser{w}, nil
	case "wav":
		if _, err := w.Write(WAVHeader(sampleRate, -1)); err != nil {
			return nil, err
		}
		return nopCloser{w}, nil
	}
	if FFmpeg() == "" {
		return nil, ErrNeedFFmpeg
	}
	args := append([]string{"-nostdin", "-v", "error", "-f", "s16le", "-ar", strconv.Itoa(sampleRate), "-ac", "1", "-i", "pipe:0"}, f.ffmpegArgs...)
	args = append(args, "pipe:1")
	cmd := exec.CommandContext(ctx, FFmpeg(), args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stderr := new(bytes.Buffer)
	cmd.Stdout, cmd.Stderr = w, stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &ffmpegEncoder{cmd: cmd, stdin: stdin, stderr: stderr}, nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

type ffmpegEncoder struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer
}

func (e *ffmpegEncoder) Write(p []byte) (int, error) { return e.stdin.Write(p) }

func (e *ffmpegEncoder) Close() error {
	e.stdin.Close()
	if err := e.cmd.Wait(); err != nil {
		return fmt.Errorf("audio encoding failed: %s", firstLine(e.stderr.String(), err))
	}
	return nil
}

func firstLine(s string, fallback error) string {
	for i, c := range s {
		if c == '\n' {
			s = s[:i]
			break
		}
	}
	if s == "" {
		return fallback.Error()
	}
	return s
}

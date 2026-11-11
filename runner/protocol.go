// Package runner defines the contract between the Oaudio daemon and an engine.
//
// A runner is a separate process that holds exactly one model in memory. The
// daemon starts it, talks to it over HTTP on the loopback interface, and kills
// it to unload the model. This is the same process model Ollama uses for its
// LLM runners; it gives crash isolation, guaranteed memory release on unload,
// and lets an engine be written in any language and under any license.
//
// The protocol is deliberately small. A runner only converts between text and
// mono 16-bit PCM. Decoding uploads, resampling, response encoding, text
// chunking, subtitles, scheduling and the public API all live in the daemon,
// so an engine adapter is a few hundred lines.
//
// See docs/runner-protocol.md for the wire format. Go engines can use Main
// and implement Synthesizer or Transcriber; anything else can speak HTTP.
package runner

import (
	"context"
	"math"
)

// ProtocolVersion is bumped on incompatible wire changes.
const ProtocolVersion = 1

// Handshake is the single JSON line a runner prints on stdout once its model
// is loaded and its HTTP server is accepting connections.
type Handshake struct {
	Protocol int    `json:"oaudio_runner"`
	Addr     string `json:"addr"` // host:port on loopback
}

// Info describes a loaded runner (GET /v1/info).
type Info struct {
	Protocol      int      `json:"protocol"`
	Engine        string   `json:"engine"`
	EngineVersion string   `json:"engine_version,omitempty"`
	Tasks         []string `json:"tasks"`                 // "stt", "tts", "vad", "diarize"
	SampleRate    int      `json:"sample_rate,omitempty"` // PCM rate produced by synthesis

	// WholeInput asks the daemon not to split synthesis input into sentences.
	// Set it when each call has a high fixed cost and the engine is fast
	// enough that splitting would only add time.
	WholeInput bool `json:"whole_input,omitempty"`
}

// Voice is one speaker (GET /v1/voices).
type Voice struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Language string `json:"language,omitempty"`
	Gender   string `json:"gender,omitempty"`
}

// SynthesisRequest is the body of POST /v1/synthesize. The response body is
// raw mono s16le PCM at Info.SampleRate, written as it is produced.
type SynthesisRequest struct {
	Text     string  `json:"text"`
	Voice    string  `json:"voice,omitempty"`
	Language string  `json:"language,omitempty"`
	Speed    float64 `json:"speed,omitempty"` // 1 is normal; 0 means default

	// Reference asks a voice-cloning model to speak in the voice of a
	// recording instead of a built-in voice.
	Reference *Reference `json:"reference,omitempty"`
}

// Reference is a recording of the voice to imitate. The daemon and its
// runners share a machine, so the audio is passed as a file path: a WAV file,
// mono, 16-bit.
type Reference struct {
	AudioPath string `json:"audio_path"`
	Text      string `json:"text,omitempty"` // what is said in the recording, if known
}

// TranscribeOptions are the query parameters of POST /v1/transcribe. The
// request body is raw mono s16le PCM at SampleRate.
type TranscribeOptions struct {
	SampleRate     int     `json:"sample_rate"`
	Language       string  `json:"language,omitempty"` // ISO 639-1, empty to detect
	Task           string  `json:"task,omitempty"`     // "transcribe" (default) or "translate"
	Prompt         string  `json:"prompt,omitempty"`
	Temperature    float64 `json:"temperature,omitempty"`
	WordTimestamps bool    `json:"word_timestamps,omitempty"`
}

// Word is a word with its time range in seconds.
type Word struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Segment is a run of recognised speech with its time range in seconds.
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
	Words []Word  `json:"words,omitempty"`
}

// Span is a stretch of audio, in seconds, that contains speech.
type Span struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// DetectOptions are the query parameters of POST /v1/vad. Zero values mean
// the model's defaults.
type DetectOptions struct {
	SampleRate   int     `json:"sample_rate"`
	Threshold    float64 `json:"threshold,omitempty"`      // speech probability, 0 to 1
	MinSilenceMS int     `json:"min_silence_ms,omitempty"` // silence that ends a span
	MinSpeechMS  int     `json:"min_speech_ms,omitempty"`  // shorter spans are dropped
}

// Turn is a stretch of audio spoken by one speaker. Speakers are numbered
// from 0 in order of first appearance.
type Turn struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker int     `json:"speaker"`
}

// DiarizeOptions are the query parameters of POST /v1/diarize.
type DiarizeOptions struct {
	SampleRate  int     `json:"sample_rate"`
	NumSpeakers int     `json:"num_speakers,omitempty"` // 0 when unknown
	Threshold   float64 `json:"threshold,omitempty"`    // clustering distance when NumSpeakers is 0
}

// Event is one line of the NDJSON response of POST /v1/transcribe: zero or
// more "segment" events, then exactly one "done" or "error".
type Event struct {
	Type     string   `json:"type"`
	Segment  *Segment `json:"segment,omitempty"`
	Language string   `json:"language,omitempty"`
	Message  string   `json:"message,omitempty"`
}

// Event types.
const (
	EventSegment = "segment"
	EventDone    = "done"
	EventError   = "error"
)

// StreamOptions are the query parameters of POST /v1/stream, which
// recognizes one utterance while it is still being spoken. The request body
// is the next piece of the utterance as mono s16le PCM at SampleRate.
type StreamOptions struct {
	Session    string `json:"session"`            // chosen by the daemon
	SampleRate int    `json:"sample_rate"`        // the manifest's audio.sample_rate
	Language   string `json:"language,omitempty"` // ISO 639-1, empty to detect; read with Start
	Start      bool   `json:"start,omitempty"`    // the first piece: start a new stream
	End        bool   `json:"end,omitempty"`      // the utterance is over: finish, answer, forget
}

// StreamResult answers POST /v1/stream with the whole transcript so far, not
// the change, so that a runner keeps no record of what it has already said.
type StreamResult struct {
	Text     string `json:"text"`
	Done     bool   `json:"done"`               // true on the answer to End
	Language string `json:"language,omitempty"` // with Done, the language heard if known
}

// Engine is implemented by every Go runner.
type Engine interface {
	Info() Info
	Close() error
}

// Synthesizer is an Engine that turns text into speech.
type Synthesizer interface {
	Engine
	Voices(ctx context.Context) ([]Voice, error)
	// Synthesize calls emit with consecutive chunks of mono s16le PCM at
	// Info().SampleRate. Emitting early chunks before the whole utterance is
	// ready is what makes streaming responses possible.
	Synthesize(ctx context.Context, req SynthesisRequest, emit func(pcm []byte) error) error
}

// Transcriber is an Engine that turns speech into text.
type Transcriber interface {
	Engine
	// Transcribe receives mono samples in [-1, 1] at opts.SampleRate, calls
	// emit once per segment in order, and returns the spoken language
	// (ISO 639-1) if known.
	Transcribe(ctx context.Context, samples []float32, opts TranscribeOptions, emit func(Segment) error) (language string, err error)
}

// Streamer is a Transcriber that can also recognize an utterance while it is
// being spoken. Models whose manifest declares the "stream" capability are
// served by one.
type Streamer interface {
	Transcriber
	// OpenStream starts recognizing one utterance.
	OpenStream(opts StreamOptions) (Stream, error)
}

// Stream is one utterance being recognized as it arrives. Calls on one
// stream never overlap, and Close is always the last call.
type Stream interface {
	// Accept adds mono samples in [-1, 1] and returns the transcript so far.
	Accept(samples []float32) (text string, err error)
	// Finish marks the end of the utterance and returns the final transcript
	// and the language heard (ISO 639-1, empty when unknown).
	Finish() (text, language string, err error)
	// Close releases the stream, finished or not.
	Close()
}

// Detector is an Engine that finds speech in audio.
type Detector interface {
	Engine
	// Detect returns the spans of speech in mono samples at opts.SampleRate,
	// in order.
	Detect(ctx context.Context, samples []float32, opts DetectOptions) ([]Span, error)
}

// Diarizer is an Engine that finds who spoke when.
type Diarizer interface {
	Engine
	// Diarize returns speaker turns in mono samples at opts.SampleRate,
	// ordered by start time.
	Diarize(ctx context.Context, samples []float32, opts DiarizeOptions) ([]Turn, error)
}

// Model is what a runner is told about the model it must load. It is the
// subset of the manifest an engine needs.
type Model struct {
	Name   string `json:"name"`
	Tag    string `json:"tag"`
	Task   string `json:"task"`
	Engine string `json:"engine"`
	Family string `json:"family"`
	Source struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	} `json:"source"`
	Audio struct {
		SampleRate int `json:"sample_rate"`
	} `json:"audio"`
	Languages    []string          `json:"languages"`
	Voices       []Voice           `json:"voices"`
	DefaultVoice string            `json:"default_voice"`
	EngineConfig map[string]string `json:"engine_config"`

	// Dir is the directory holding the files, with upstream names.
	Dir string `json:"-"`
}

// PCM16 converts samples in [-1, 1] to the little-endian 16-bit PCM bytes the
// protocol carries. Engines that produce float audio call it before emit.
func PCM16(samples []float32) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := int32(math.Round(float64(s) * 32768))
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		out[i*2], out[i*2+1] = byte(v), byte(v>>8)
	}
	return out
}

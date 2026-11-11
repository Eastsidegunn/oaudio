package runner

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Main is the entry point of a Go runner. It parses the standard runner
// flags, loads the model through load, and serves the protocol until the
// daemon closes the runner's stdin or sends SIGTERM.
//
//	func main() { runner.Main(os.Args[1:], func(m runner.Model) (runner.Engine, error) { ... }) }
func Main(args []string, load func(Model) (Engine, error)) {
	fs := flag.NewFlagSet("runner", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "path to the model manifest (JSON)")
	modelDir := fs.String("model-dir", "", "directory holding the model files")
	fs.Parse(args)

	var m Model
	if *manifestPath != "" {
		data, err := os.ReadFile(*manifestPath)
		if err != nil {
			fatal(err)
		}
		if err := json.Unmarshal(data, &m); err != nil {
			fatal(fmt.Errorf("manifest: %w", err))
		}
	}
	m.Dir = *modelDir

	engine, err := load(m)
	if err != nil {
		fatal(fmt.Errorf("load %s:%s: %w", m.Name, m.Tag, err))
	}
	if err := Serve(context.Background(), engine, os.Stdin, os.Stdout); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "oaudio runner:", err)
	os.Exit(1)
}

// Serve runs the protocol for engine. It writes the handshake line to
// handshake, then blocks until parent reaches EOF (the daemon went away),
// a termination signal arrives, or ctx is cancelled.
func Serve(ctx context.Context, engine Engine, parent io.Reader, handshake io.Writer) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: Handler(engine)}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	if err := json.NewEncoder(handshake).Encode(Handshake{Protocol: ProtocolVersion, Addr: ln.Addr().String()}); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	gone := make(chan struct{})
	if parent != nil {
		go func() {
			io.Copy(io.Discard, parent) // returns when the daemon closes our stdin
			close(gone)
		}()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	case <-gone:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	return engine.Close()
}

// Handler returns the HTTP handler implementing the runner protocol for
// engine. Engines are not assumed to be reentrant: requests run one at a time.
func Handler(engine Engine) http.Handler {
	var mu sync.Mutex
	mux := http.NewServeMux()
	// fail answers a request that failed before its response started.
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusInternalServerError
		var bad *BadRequestError
		if errors.As(err, &bad) {
			status = http.StatusBadRequest
		}
		httpError(w, status, err.Error())
	}

	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		info := engine.Info()
		info.Protocol = ProtocolVersion
		writeJSON(w, http.StatusOK, info)
	})

	mux.HandleFunc("GET /v1/voices", func(w http.ResponseWriter, r *http.Request) {
		s, ok := engine.(Synthesizer)
		if !ok {
			httpError(w, http.StatusNotImplemented, "this runner does not synthesize speech")
			return
		}
		voices, err := s.Voices(r.Context())
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if voices == nil {
			voices = []Voice{}
		}
		writeJSON(w, http.StatusOK, voices)
	})

	mux.HandleFunc("POST /v1/synthesize", func(w http.ResponseWriter, r *http.Request) {
		s, ok := engine.(Synthesizer)
		if !ok {
			httpError(w, http.StatusNotImplemented, "this runner does not synthesize speech")
			return
		}
		var req SynthesisRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		mu.Lock()
		defer mu.Unlock()
		started := false
		flusher, _ := w.(http.Flusher)
		err := s.Synthesize(r.Context(), req, func(pcm []byte) error {
			if !started {
				w.Header().Set("Content-Type", "audio/pcm")
				w.Header().Set("X-Sample-Rate", strconv.Itoa(engine.Info().SampleRate))
				w.WriteHeader(http.StatusOK)
				started = true
			}
			if _, err := w.Write(pcm); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		})
		switch {
		case err != nil && !started:
			status := http.StatusInternalServerError
			var bad *BadRequestError
			if errors.As(err, &bad) {
				status = http.StatusBadRequest
			}
			httpError(w, status, err.Error())
		case err != nil:
			// Headers are gone; abort the connection so the daemon sees a
			// truncated body instead of mistaking it for a complete one.
			panic(http.ErrAbortHandler)
		case !started:
			w.Header().Set("Content-Type", "audio/pcm")
			w.Header().Set("X-Sample-Rate", strconv.Itoa(engine.Info().SampleRate))
			w.WriteHeader(http.StatusOK)
		}
	})

	mux.HandleFunc("POST /v1/transcribe", func(w http.ResponseWriter, r *http.Request) {
		t, ok := engine.(Transcriber)
		if !ok {
			httpError(w, http.StatusNotImplemented, "this runner does not transcribe speech")
			return
		}
		q := r.URL.Query()
		opts := TranscribeOptions{
			Language:       q.Get("language"),
			Task:           q.Get("task"),
			Prompt:         q.Get("prompt"),
			WordTimestamps: q.Get("word_timestamps") == "1",
		}
		opts.SampleRate, _ = strconv.Atoi(q.Get("sample_rate"))
		opts.Temperature, _ = strconv.ParseFloat(q.Get("temperature"), 64)
		if opts.SampleRate <= 0 {
			httpError(w, http.StatusBadRequest, "sample_rate is required")
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		samples := make([]float32, len(raw)/2)
		for i := range samples {
			samples[i] = float32(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
		}
		mu.Lock()
		defer mu.Unlock()
		// The status is sent with the first event, so that an engine that
		// refuses the request before recognizing anything can still answer
		// with 400 or 500 instead of an error event under a 200.
		started := false
		enc := json.NewEncoder(w)
		flusher, _ := w.(http.Flusher)
		send := func(ev Event) error {
			if !started {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			if err := enc.Encode(ev); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		}
		lang, err := t.Transcribe(r.Context(), samples, opts, func(s Segment) error {
			return send(Event{Type: EventSegment, Segment: &s})
		})
		switch {
		case err != nil && !started:
			fail(w, err)
		case err != nil:
			send(Event{Type: EventError, Message: err.Error()})
		default:
			send(Event{Type: EventDone, Language: lang})
		}
	})
	// readPCM decodes the s16le request body shared by the audio-in endpoints.
	readPCM := func(w http.ResponseWriter, r *http.Request) (samples []float32, rate int, ok bool) {
		rate, _ = strconv.Atoi(r.URL.Query().Get("sample_rate"))
		if rate <= 0 {
			httpError(w, http.StatusBadRequest, "sample_rate is required")
			return nil, 0, false
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return nil, 0, false
		}
		samples = make([]float32, len(raw)/2)
		for i := range samples {
			samples[i] = float32(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
		}
		return samples, rate, true
	}

	mux.HandleFunc("POST /v1/vad", func(w http.ResponseWriter, r *http.Request) {
		d, ok := engine.(Detector)
		if !ok {
			httpError(w, http.StatusNotImplemented, "this runner does not detect speech")
			return
		}
		samples, rate, ok := readPCM(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		opts := DetectOptions{SampleRate: rate}
		opts.Threshold, _ = strconv.ParseFloat(q.Get("threshold"), 64)
		opts.MinSilenceMS, _ = strconv.Atoi(q.Get("min_silence_ms"))
		opts.MinSpeechMS, _ = strconv.Atoi(q.Get("min_speech_ms"))
		mu.Lock()
		defer mu.Unlock()
		spans, err := d.Detect(r.Context(), samples, opts)
		if err != nil {
			fail(w, err)
			return
		}
		if spans == nil {
			spans = []Span{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"spans": spans})
	})

	mux.HandleFunc("POST /v1/diarize", func(w http.ResponseWriter, r *http.Request) {
		d, ok := engine.(Diarizer)
		if !ok {
			httpError(w, http.StatusNotImplemented, "this runner does not diarize")
			return
		}
		samples, rate, ok := readPCM(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		opts := DiarizeOptions{SampleRate: rate}
		opts.NumSpeakers, _ = strconv.Atoi(q.Get("num_speakers"))
		opts.Threshold, _ = strconv.ParseFloat(q.Get("threshold"), 64)
		mu.Lock()
		defer mu.Unlock()
		turns, err := d.Diarize(r.Context(), samples, opts)
		if err != nil {
			fail(w, err)
			return
		}
		if turns == nil {
			turns = []Turn{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"turns": turns})
	})
	return mux
}

// BadRequestError marks an engine error caused by the request (an unknown
// voice, an unsupported language) rather than by the engine failing.
type BadRequestError struct{ Message string }

func (e *BadRequestError) Error() string { return e.Message }

// BadRequest builds a BadRequestError.
func BadRequest(format string, a ...any) error {
	return &BadRequestError{Message: fmt.Sprintf(format, a...)}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

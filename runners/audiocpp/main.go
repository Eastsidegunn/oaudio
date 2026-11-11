//go:build unix

// Command oaudio-runner-audio.cpp runs GGUF speech models through audio.cpp
// (https://github.com/0xShug0/audio.cpp, Apache-2.0).
//
// audio.cpp ships its own HTTP server, so this runner links none of it. It
// starts `audiocpp_server` with a configuration for the one model it was
// asked to load, and translates the Oaudio runner contract into requests to
// that server. All of the modelling is audio.cpp's work.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/eastsidegunn/oaudio/runner"
)

// engineVersion is the audio.cpp tag the Makefile built, set at link time.
var engineVersion = "unknown"

// The server is started through a shell so that it cannot outlive this
// process: the shell holds our pipe as descriptor 3 and stops the server the
// moment the pipe closes, which the kernel does for us even if we are killed.
const supervise = `exec 3<&0
"$@" &
pid=$!
(cat <&3 >/dev/null; kill "$pid" 2>/dev/null) &
wait "$pid"`

type engine struct {
	base   string // http://127.0.0.1:port
	client *http.Client
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	dir    string // temporary directory holding the server configuration
	done   chan struct{}
	closed bool

	rate       int
	streaming  bool
	voices     []runner.Voice
	defVoice   string
	refText    string // "required", "optional" or "never"
	noRefOK    bool   // the model can speak without a reference recording
	langs      map[string]bool
	reqOptions map[string]any
	// noTextOptions are added to a cloning request whose voice has no
	// transcript, for models that need to be told to do without one.
	noTextOptions map[string]any
}

func serverBinary() (string, error) {
	if p := os.Getenv("OAUDIO_AUDIOCPP_SERVER"); p != "" {
		return p, nil
	}
	self, err := os.Executable()
	if err == nil {
		p := filepath.Join(filepath.Dir(self), "audiocpp_server")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("audiocpp_server"); err == nil {
		return p, nil
	}
	return "", errors.New("audiocpp_server not found: build it with `make -C runners/audiocpp`, or set OAUDIO_AUDIOCPP_SERVER")
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func jsonObject(field, text string) (map[string]any, error) {
	out := map[string]any{}
	if text == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("engine_config.%s is not a JSON object: %w", field, err)
	}
	return out, nil
}

func load(m runner.Model) (runner.Engine, error) {
	if m.Task != "tts" {
		return nil, fmt.Errorf("this runner does not support task %q yet", m.Task)
	}
	cfg := m.EngineConfig
	if cfg["family"] == "" || cfg["model"] == "" {
		return nil, errors.New("the manifest's engine_config needs audio.cpp's `family` and the `model` file")
	}
	bin, err := serverBinary()
	if err != nil {
		return nil, err
	}
	sessionOptions, err := jsonObject("session_options", cfg["session_options"])
	if err != nil {
		return nil, err
	}
	requestOptions, err := jsonObject("request_options", cfg["request_options"])
	if err != nil {
		return nil, err
	}
	noTextOptions, err := jsonObject("no_reference_text_options", cfg["no_reference_text_options"])
	if err != nil {
		return nil, err
	}
	backend := os.Getenv("OAUDIO_AUDIOCPP_BACKEND")
	if backend == "" {
		backend = cfg["backend"]
	}
	if backend == "" {
		backend = "cpu"
		if runtime.GOOS == "darwin" {
			backend = "metal"
		}
	}
	threads := runtime.NumCPU()
	if threads > 8 {
		threads = 8
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	mode := "offline"
	if cfg["mode"] == "streaming" {
		mode = "streaming"
	}
	model := map[string]any{
		"id":     "model",
		"family": cfg["family"],
		"path":   filepath.Join(m.Dir, filepath.FromSlash(cfg["model"])),
		"task":   "tts",
		"mode":   mode,
	}
	if len(sessionOptions) > 0 {
		model["session_options"] = sessionOptions
	}
	conf, _ := json.Marshal(map[string]any{
		"host": "127.0.0.1", "port": port, "backend": backend, "threads": threads,
		"lazy_load": false, "ui": false, "models": []any{model},
	})
	dir, err := os.MkdirTemp("", "oaudio-audiocpp-")
	if err != nil {
		return nil, err
	}
	confPath := filepath.Join(dir, "server.json")
	if err := os.WriteFile(confPath, conf, 0o600); err != nil {
		return nil, err
	}

	e := &engine{
		base:   "http://127.0.0.1:" + strconv.Itoa(port),
		client: &http.Client{},
		dir:    dir,
		done:   make(chan struct{}),

		rate:       m.Audio.SampleRate,
		streaming:  mode == "streaming",
		voices:     m.Voices,
		defVoice:   m.DefaultVoice,
		refText:    cfg["reference_text"],
		noRefOK:    cfg["reference"] == "optional",
		reqOptions: requestOptions,

		noTextOptions: noTextOptions,
	}
	if e.rate <= 0 {
		return nil, errors.New("the manifest must give audio.sample_rate for this engine")
	}
	if list := cfg["languages"]; list != "" {
		e.langs = map[string]bool{}
		for _, l := range strings.Split(list, ",") {
			e.langs[strings.TrimSpace(l)] = true
		}
	}

	e.cmd = exec.Command("/bin/sh", "-c", supervise, "sh", bin, "--config", confPath, "--no-ui")
	e.cmd.Stdout, e.cmd.Stderr = os.Stderr, os.Stderr
	if e.stdin, err = e.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if err := e.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		e.cmd.Wait()
		close(e.done)
		if !e.closed {
			// The server died under us. Go with it, so the daemon sees a
			// crashed runner instead of one that fails every request.
			fmt.Fprintln(os.Stderr, "oaudio runner: audiocpp_server exited")
			os.Exit(1)
		}
	}()

	// The server answers /health once the model is in memory.
	deadline := time.Now().Add(10 * time.Minute)
	for {
		resp, err := e.client.Get(e.base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return e, nil
			}
		}
		select {
		case <-e.done:
			return nil, errors.New("audiocpp_server exited while loading the model (see the messages above)")
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			e.Close()
			return nil, errors.New("audiocpp_server did not become ready in ten minutes")
		}
	}
}

func (e *engine) Info() runner.Info {
	return runner.Info{Engine: "audio.cpp", EngineVersion: engineVersion, Tasks: []string{"tts"}, SampleRate: e.rate,
		// audio.cpp splits long text itself, and a cloning request pays for
		// its reference recording every time it is called.
		WholeInput: true}
}

func (e *engine) Close() error {
	e.closed = true
	e.stdin.Close() // the supervising shell stops the server
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		e.cmd.Process.Kill()
	}
	return os.RemoveAll(e.dir)
}

func (e *engine) Voices(context.Context) ([]runner.Voice, error) { return e.voices, nil }

func (e *engine) Synthesize(ctx context.Context, req runner.SynthesisRequest, emit func([]byte) error) error {
	body := map[string]any{"model": "model", "input": req.Text}
	if lang := strings.ToLower(req.Language); lang != "" {
		if i := strings.IndexAny(lang, "-_"); i > 0 {
			lang = lang[:i]
		}
		if e.langs != nil && !e.langs[lang] {
			return runner.BadRequest("this model does not speak %q", req.Language)
		}
		body["language"] = lang
	}
	options := map[string]any{}
	for k, v := range e.reqOptions {
		options[k] = v
	}
	switch {
	case req.Reference != nil:
		body["voice_ref"] = req.Reference.AudioPath
		switch {
		case e.refText == "never":
		case req.Reference.Text != "":
			body["reference_text"] = req.Reference.Text
		case e.refText != "required":
			for k, v := range e.noTextOptions {
				options[k] = v
			}
		default:
			return runner.BadRequest("this model needs to know what is said in the reference recording: recreate the voice with a transcript (`oaudio voice create --transcript ...` or `--transcribe-with <model>`)")
		}
	case len(e.voices) > 0:
		name := req.Voice
		if name == "" {
			name = e.defVoice
		}
		found := false
		for _, v := range e.voices {
			if strings.EqualFold(v.ID, name) {
				name, found = v.ID, true
			}
		}
		if !found {
			return runner.BadRequest("unknown voice %q", req.Voice)
		}
		if name != "default" {
			body["voice"] = name
		}
	case !e.noRefOK:
		return runner.BadRequest("this model has no built-in voices: create one with `oaudio voice create` and pass it as the voice")
	}
	if e.streaming {
		body["response_format"], body["stream_format"] = "pcm", "audio"
	}
	if len(options) > 0 {
		body["options"] = options
	}
	payload, _ := json.Marshal(body)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.base+"/v1/audio/speech", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		text := serverMessage(msg)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return runner.BadRequest("audio.cpp: %s", text)
		}
		return fmt.Errorf("audio.cpp answered %d: %s", resp.StatusCode, text)
	}
	if !e.streaming {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		pcm, rate, err := wavPCM16(data)
		if err != nil {
			return err
		}
		if rate != e.rate {
			return fmt.Errorf("audio.cpp produced %d Hz audio, the manifest says %d", rate, e.rate)
		}
		return emit(pcm)
	}
	// Raw 16-bit PCM as it is generated. Pass it on in whole samples.
	buf := make([]byte, 32<<10)
	held := 0
	for {
		n, err := resp.Body.Read(buf[held:])
		n += held
		whole := n &^ 1
		if whole > 0 {
			if eerr := emit(append([]byte(nil), buf[:whole]...)); eerr != nil {
				return eerr
			}
		}
		if held = n - whole; held == 1 {
			buf[0] = buf[whole]
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// serverMessage pulls the human-readable part out of an error body.
func serverMessage(body []byte) string {
	var parsed struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != nil {
		switch v := parsed.Error.(type) {
		case string:
			return v
		case map[string]any:
			if s, ok := v["message"].(string); ok {
				return s
			}
		}
	}
	return strings.TrimSpace(string(body))
}

// wavPCM16 returns the samples of a mono WAV file as 16-bit PCM.
func wavPCM16(data []byte) (pcm []byte, rate int, err error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, 0, errors.New("audio.cpp did not return a WAV file")
	}
	var format, channels, bits int
	for pos := 12; pos+8 <= len(data); {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4:]))
		chunk := data[pos+8:]
		if size < len(chunk) {
			chunk = chunk[:size]
		}
		switch id {
		case "fmt ":
			if len(chunk) < 16 {
				return nil, 0, errors.New("audio.cpp returned a WAV file with a short header")
			}
			format = int(binary.LittleEndian.Uint16(chunk))
			channels = int(binary.LittleEndian.Uint16(chunk[2:]))
			rate = int(binary.LittleEndian.Uint32(chunk[4:]))
			bits = int(binary.LittleEndian.Uint16(chunk[14:]))
		case "data":
			if channels != 1 {
				return nil, 0, fmt.Errorf("audio.cpp returned %d-channel audio; expected mono", channels)
			}
			switch {
			case format == 1 && bits == 16:
				return chunk[:len(chunk)&^1], rate, nil
			case format == 3 && bits == 32:
				samples := make([]float32, len(chunk)/4)
				for i := range samples {
					samples[i] = math.Float32frombits(binary.LittleEndian.Uint32(chunk[i*4:]))
				}
				return runner.PCM16(samples), rate, nil
			}
			return nil, 0, fmt.Errorf("audio.cpp returned WAV format %d with %d bits; expected 16-bit PCM or 32-bit float", format, bits)
		}
		pos += 8 + size + size&1
	}
	return nil, 0, errors.New("audio.cpp returned a WAV file with no audio in it")
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("oaudio-runner-audio.cpp, audio.cpp", engineVersion)
		return
	}
	runner.Main(os.Args[1:], load)
}

// Package apple adapts the speech engines that ship with macOS to the runner
// contract: the system voices (through /usr/bin/say) for synthesis and the
// on-device SpeechAnalyzer for recognition. Nothing is downloaded by Oaudio
// and audio never leaves the machine.
package apple

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/eastsidegunn/oaudio/internal/audio"
	"github.com/eastsidegunn/oaudio/runner"
)

const ttsSampleRate = 22050

// New loads the engine for the task named in the manifest.
func New(m runner.Model) (runner.Engine, error) {
	version := "macOS"
	if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
		version = "macOS " + strings.TrimSpace(string(out))
	}
	switch m.Task {
	case "tts":
		voices, err := listVoices()
		if err != nil {
			return nil, err
		}
		return &tts{version: version, voices: voices, defaultVoice: m.DefaultVoice}, nil
	case "stt":
		helper, err := findHelper()
		if err != nil {
			return nil, err
		}
		s := &stt{version: version, helper: helper, defaultLocale: m.EngineConfig["default_locale"]}
		if s.defaultLocale == "" {
			s.defaultLocale = "en-US"
		}
		if err := s.start(); err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, fmt.Errorf("apple engine: unsupported task %q", m.Task)
}

// ---- synthesis ----

type tts struct {
	version      string
	voices       []runner.Voice
	defaultVoice string
}

func (t *tts) Info() runner.Info {
	// Each call to say costs about 0.4 s before any audio exists, then runs far
	// faster than real time, so one call for the whole input is quickest.
	return runner.Info{Engine: "apple", EngineVersion: t.version, Tasks: []string{"tts"}, SampleRate: ttsSampleRate, WholeInput: true}
}

func (t *tts) Close() error { return nil }

func (t *tts) Voices(context.Context) ([]runner.Voice, error) { return t.voices, nil }

var voiceLine = regexp.MustCompile(`^(.+?)\s+([a-z]{2,3}_[A-Za-z0-9]{2,})\s+#`)

func listVoices() ([]runner.Voice, error) {
	out, err := exec.Command("/usr/bin/say", "-v", "?").Output()
	if err != nil {
		return nil, fmt.Errorf("list system voices: %w", err)
	}
	var voices []runner.Voice
	for _, line := range strings.Split(string(out), "\n") {
		m := voiceLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id := strings.TrimSpace(m[1])
		voices = append(voices, runner.Voice{ID: id, Name: id, Language: strings.ReplaceAll(m[2], "_", "-")})
	}
	if len(voices) == 0 {
		return nil, errors.New("no system voices found")
	}
	return voices, nil
}

// pickVoice resolves a requested voice. Names match case-insensitively, and
// "Eddy" finds "Eddy (English (US))". With no voice, a language picks the
// first voice that speaks it.
func (t *tts) pickVoice(want, language string) (string, error) {
	if want == "" && language == "" {
		want = t.defaultVoice
	}
	if want != "" {
		lw := strings.ToLower(want)
		var prefixMatch string
		for _, v := range t.voices {
			lv := strings.ToLower(v.ID)
			if lv == lw {
				return v.ID, nil
			}
			if prefixMatch == "" && strings.HasPrefix(lv, lw+" (") {
				if language == "" || strings.HasPrefix(strings.ToLower(v.Language), strings.ToLower(language)) {
					prefixMatch = v.ID
				}
			}
		}
		if prefixMatch != "" {
			return prefixMatch, nil
		}
		if want == t.defaultVoice {
			return "", nil // manifest default not installed: use the system default voice
		}
		return "", runner.BadRequest("unknown voice %q; list voices with `oaudio show apple-say --voices`", want)
	}
	if language != "" {
		for _, v := range t.voices {
			if strings.HasPrefix(strings.ToLower(v.Language), strings.ToLower(language)) {
				return v.ID, nil
			}
		}
		return "", runner.BadRequest("no installed system voice speaks %q", language)
	}
	return "", nil
}

func (t *tts) Synthesize(ctx context.Context, req runner.SynthesisRequest, emit func([]byte) error) error {
	voice, err := t.pickVoice(req.Voice, req.Language)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "oaudio-say-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	textPath, wavPath := filepath.Join(dir, "in.txt"), filepath.Join(dir, "out.wav")
	// Text goes through a file so it can never be read as an option.
	if err := os.WriteFile(textPath, []byte(req.Text), 0o600); err != nil {
		return err
	}
	args := []string{"-o", wavPath, "--file-format=WAVE", "--data-format=LEI16@" + strconv.Itoa(ttsSampleRate), "-f", textPath}
	if voice != "" {
		args = append(args, "-v", voice)
	}
	if req.Speed > 0 && req.Speed != 1 {
		// say takes words per minute; system voices default to about 175.
		args = append(args, "-r", strconv.Itoa(int(175*req.Speed)))
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/say", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("say: %v: %s", err, strings.TrimSpace(string(out)))
	}
	data, err := os.ReadFile(wavPath)
	if err != nil {
		return err
	}
	samples, rate, err := audio.DecodeWAV(data)
	if err != nil {
		return err
	}
	return emit(audio.FloatToS16(audio.Resample(samples, rate, ttsSampleRate)))
}

// ---- recognition ----

type stt struct {
	version       string
	helper        string
	defaultLocale string

	mu    sync.Mutex
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Scanner
}

func (s *stt) Info() runner.Info {
	return runner.Info{Engine: "apple", EngineVersion: s.version, Tasks: []string{"stt"}}
}

// findHelper locates the oaudio-apple-stt binary built from stt.swift.
func findHelper() (string, error) {
	const name = "oaudio-apple-stt"
	var candidates []string
	if p := os.Getenv("OAUDIO_APPLE_STT"); p != "" {
		candidates = append(candidates, p)
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, name), filepath.Join(dir, "..", "libexec", "oaudio", name))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("%s not found next to the oaudio binary; build it with `make` (needs the Xcode command line tools)", name)
}

func (s *stt) start() error {
	cmd := exec.Command(s.helper)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 16<<20)
	s.cmd, s.stdin, s.out = cmd, stdin, sc
	return nil
}

func (s *stt) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		s.stdin.Close()
		s.cmd.Wait()
		s.cmd = nil
	}
	return nil
}

// A bare language code needs a region for SpeechTranscriber.
var defaultRegion = map[string]string{
	"en": "en-US", "ko": "ko-KR", "ja": "ja-JP", "zh": "zh-CN", "yue": "yue-CN",
	"es": "es-ES", "fr": "fr-FR", "de": "de-DE", "it": "it-IT", "pt": "pt-BR",
}

func (s *stt) Transcribe(ctx context.Context, samples []float32, opts runner.TranscribeOptions, emit func(runner.Segment) error) (string, error) {
	if opts.Task == "translate" {
		return "", runner.BadRequest("the apple engine does not translate")
	}
	locale := opts.Language
	switch {
	case locale == "":
		locale = s.defaultLocale
	case !strings.ContainsAny(locale, "-_"):
		if full, ok := defaultRegion[strings.ToLower(locale)]; ok {
			locale = full
		}
	}
	tmp, err := os.CreateTemp("", "oaudio-stt-*.wav")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := audio.WriteWAV(tmp, audio.FloatToS16(samples), opts.SampleRate); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		if err := s.start(); err != nil {
			return "", err
		}
	}
	req, _ := json.Marshal(map[string]string{"op": "transcribe", "path": tmp.Name(), "locale": locale})
	if _, err := s.stdin.Write(append(req, '\n')); err != nil {
		s.kill()
		return "", fmt.Errorf("speech helper is gone: %w", err)
	}
	for s.out.Scan() {
		var msg struct {
			Segment *runner.Segment `json:"segment"`
			Done    bool            `json:"done"`
			Error   string          `json:"error"`
		}
		if err := json.Unmarshal(s.out.Bytes(), &msg); err != nil {
			continue
		}
		switch {
		case msg.Error != "":
			return "", errors.New(msg.Error)
		case msg.Done:
			lang, _, _ := strings.Cut(strings.ReplaceAll(locale, "_", "-"), "-")
			return strings.ToLower(lang), nil
		case msg.Segment != nil:
			if !opts.WordTimestamps {
				msg.Segment.Words = nil
			}
			if err := emit(*msg.Segment); err != nil {
				// The helper is mid-request; restart it rather than desynchronise.
				s.kill()
				return "", err
			}
		}
	}
	s.kill()
	return "", errors.New("speech helper exited unexpectedly")
}

func (s *stt) kill() {
	if s.cmd != nil {
		s.stdin.Close()
		s.cmd.Process.Kill()
		s.cmd.Wait()
		s.cmd = nil
	}
}

// Package server is the Oaudio daemon: an OpenAI-compatible speech API plus
// a small native API for model management.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eastsidegunn/oaudio/internal/catalog"
	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/internal/pull"
	"github.com/eastsidegunn/oaudio/internal/sched"
	"github.com/eastsidegunn/oaudio/internal/store"
	"github.com/eastsidegunn/oaudio/internal/voices"
	"github.com/eastsidegunn/oaudio/runner"
)

// DefaultAddr is where the daemon listens unless OAUDIO_HOST says otherwise.
const DefaultAddr = "127.0.0.1:11535"

// Addr returns the configured listen/connect address.
func Addr() string {
	h := strings.TrimPrefix(strings.TrimPrefix(os.Getenv("OAUDIO_HOST"), "http://"), "https://")
	h = strings.TrimRight(h, "/")
	switch {
	case h == "":
		return DefaultAddr
	case !strings.Contains(h, ":"):
		return h + ":11535"
	}
	return h
}

// BuiltinEngines are runners compiled into the oaudio binary and started as
// `oaudio runner <engine>`. Every other engine is a separate executable named
// oaudio-runner-<engine>.
var BuiltinEngines = map[string]bool{"apple": true, "reference": true}

// Config configures a Server.
type Config struct {
	Version   string
	Store     *store.Store
	Catalog   *catalog.Catalog
	MaxLoaded int
	KeepAlive time.Duration
	Aliases   map[string]string // request model name -> name:tag
	Launch    sched.LaunchFunc  // nil: start real runner processes
	Log       *slog.Logger
}

// Server holds daemon state.
type Server struct {
	cfg    Config
	sched  *sched.Scheduler
	voices *voices.Store
	log    *slog.Logger

	pullMu sync.Mutex
	pulls  map[string]*sync.Mutex
}

// New builds a Server.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg, log: cfg.Log, pulls: map[string]*sync.Mutex{}, voices: voices.New(cfg.Store.Home)}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	launch := cfg.Launch
	if launch == nil {
		launch = s.launchProcess
	}
	s.sched = sched.New(launch, cfg.MaxLoaded, cfg.KeepAlive)
	return s
}

// Close unloads every model.
func (s *Server) Close() { s.sched.Shutdown() }

// Handler returns the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "Oaudio is running") })

	// OpenAI-compatible surface.
	mux.HandleFunc("POST /v1/audio/speech", s.handleSpeech)
	mux.HandleFunc("POST /v1/audio/transcriptions", s.handleTranscription(false))
	mux.HandleFunc("POST /v1/audio/translations", s.handleTranscription(true))
	mux.HandleFunc("POST /v1/audio/voices", s.handleVoiceCreate)
	mux.HandleFunc("GET /v1/realtime", s.handleRealtime)
	// Extensions in the same style, for things OpenAI's API has no endpoint for.
	mux.HandleFunc("GET /v1/audio/voices", s.handleVoiceList)
	mux.HandleFunc("DELETE /v1/audio/voices/{id}", s.handleVoiceDelete)
	mux.HandleFunc("POST /v1/audio/vad", s.handleVAD)
	mux.HandleFunc("POST /v1/audio/diarizations", s.handleDiarize)
	mux.HandleFunc("GET /v1/models", s.handleOpenAIModels)
	mux.HandleFunc("GET /v1/models/{id}", s.handleOpenAIModel)

	// Native surface.
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Version})
	})
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("GET /api/catalog", s.handleCatalog)
	mux.HandleFunc("GET /api/show", s.handleShow)
	mux.HandleFunc("GET /api/voices", s.handleVoices)
	mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"models": s.sched.Ps()})
	})
	mux.HandleFunc("POST /api/pull", s.handlePull)
	mux.HandleFunc("POST /api/stop", s.handleStop)
	mux.HandleFunc("DELETE /api/models", s.handleDelete)

	return s.logRequests(mux)
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, which the
// WebSocket upgrade needs in order to take over the connection.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status == 0 && r.Header.Get("Upgrade") != "" {
			sw.status = http.StatusSwitchingProtocols // the connection was taken over
		}
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "bytes", sw.bytes, "duration", time.Since(start).Round(time.Millisecond))
	})
}

// ---- model resolution ----

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errf(status int, format string, a ...any) *apiError {
	return &apiError{status: status, msg: fmt.Sprintf(format, a...)}
}

// lookup resolves a request's model name. Installed manifests are consulted
// first so a model keeps working even when the catalog no longer lists it.
func (s *Server) lookup(name string) (*manifest.Manifest, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errf(http.StatusBadRequest, "model is required")
	}
	if a, ok := s.cfg.Aliases[name]; ok {
		name = a
	}
	n, tag, err := manifest.SplitRef(name)
	if err != nil {
		return nil, errf(http.StatusBadRequest, "%v", err)
	}
	installed, _ := s.cfg.Store.Installed()
	for _, m := range installed {
		if m.Name == n && m.Tag == tag {
			return m, nil
		}
	}
	for _, m := range installed {
		if m.Name != n {
			continue
		}
		for _, a := range m.Aliases {
			if a == tag {
				return m, nil
			}
		}
	}
	m, err := s.cfg.Catalog.Resolve(name)
	if err != nil {
		return nil, errf(http.StatusNotFound, "%v", err)
	}
	return m, nil
}

// ready resolves a model that must be runnable for the given task.
func (s *Server) ready(name, task string) (*manifest.Manifest, error) {
	m, err := s.lookup(name)
	if err != nil {
		return nil, err
	}
	if m.Task != task {
		kind := map[string]string{
			manifest.TaskSTT: "transcribe speech", manifest.TaskTTS: "synthesize speech",
			manifest.TaskVAD: "detect speech", manifest.TaskDiarize: "tell speakers apart",
		}[task]
		return nil, errf(http.StatusBadRequest, "model %q does not %s (it is a %s model)", m.Ref(), kind, m.Task)
	}
	if !catalog.SupportsPlatform(m) {
		return nil, errf(http.StatusBadRequest, "model %q does not run on this platform", m.Ref())
	}
	if !s.cfg.Store.IsInstalled(m) {
		return nil, errf(http.StatusNotFound, "model %q is not installed; run `oaudio pull %s`", m.Ref(), m.Ref())
	}
	return m, nil
}

// ---- launching runners ----

func (s *Server) runnerCommand(m *manifest.Manifest) (exe string, args []string, err error) {
	if BuiltinEngines[m.Engine] {
		exe, err = os.Executable()
		if err != nil {
			return "", nil, err
		}
		return exe, []string{"runner", m.Engine}, nil
	}
	exe, err = FindRunner(m.Engine, s.cfg.Store.Home)
	return exe, nil, err
}

// FindRunner locates the executable for an external engine.
func FindRunner(engine, home string) (string, error) {
	name := "oaudio-runner-" + engine
	var dirs []string
	if d := os.Getenv("OAUDIO_RUNNERS"); d != "" {
		dirs = append(dirs, filepath.SplitList(d)...)
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dirs = append(dirs, filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "..", "libexec", "oaudio"))
	}
	dirs = append(dirs, filepath.Join(home, "runners"))
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("the %q engine is not installed: no %s executable found (see docs/engines.md)", engine, name)
}

func (s *Server) launchProcess(ctx context.Context, m *manifest.Manifest) (*sched.Loaded, error) {
	exe, args, err := s.runnerCommand(m)
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(s.cfg.Store.Home, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(runDir, m.Name+"-"+m.Tag+".json")
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		return nil, err
	}
	args = append(args, "--manifest", manifestPath, "--model-dir", s.cfg.Store.ModelDir(m))
	start := time.Now()
	p, err := runner.Launch(ctx, exe, args, &runnerLog{log: s.log, model: m.Ref()}, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("could not load %s: %w", m.Ref(), err)
	}
	s.log.Info("model loaded", "model", m.Ref(), "engine", p.Info.Engine, "engine_version", p.Info.EngineVersion, "pid", p.Pid(), "took", time.Since(start).Round(time.Millisecond))
	return &sched.Loaded{
		Conn: p.Conn,
		Stop: func() {
			p.Stop()
			s.log.Info("model unloaded", "model", m.Ref())
		},
		Exited: p.Exited(),
		Pid:    p.Pid(),
	}, nil
}

// runnerLog forwards a runner's stderr into the daemon log, line by line.
type runnerLog struct {
	log   *slog.Logger
	model string
	buf   []byte
}

func (l *runnerLog) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			break
		}
		if line := strings.TrimSpace(string(l.buf[:i])); line != "" {
			l.log.Info("runner", "model", l.model, "msg", line)
		}
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes an error in the shape OpenAI clients parse.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var ae *apiError
	var re *runner.Error
	switch {
	case errors.As(err, &ae):
		status = ae.status
	case errors.As(err, &re):
		status = re.Status
		if status == http.StatusNotImplemented {
			status = http.StatusBadRequest
		}
	case errors.Is(err, context.Canceled):
		status = 499
	}
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "server_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": err.Error(), "type": kind, "param": nil, "code": nil,
	}})
}

// ParseKeepAlive reads a keep-alive the way Ollama does: a number of seconds
// or a duration string such as "10m"; negative means forever.
func ParseKeepAlive(raw string) (*time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var d time.Duration
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		d = time.Duration(n * float64(time.Second))
	} else if d, err = time.ParseDuration(raw); err != nil {
		return nil, fmt.Errorf("invalid keep_alive %q: use seconds or a duration such as 10m", raw)
	}
	if d < 0 {
		d = sched.Forever
	}
	return &d, nil
}

func (s *Server) pullLock(ref string) *sync.Mutex {
	s.pullMu.Lock()
	defer s.pullMu.Unlock()
	if s.pulls[ref] == nil {
		s.pulls[ref] = &sync.Mutex{}
	}
	return s.pulls[ref]
}

// ---- native handlers ----

type modelSummary struct {
	Name        string           `json:"name"`
	Task        string           `json:"task"`
	Engine      string           `json:"engine"`
	Family      string           `json:"family,omitempty"`
	Description string           `json:"description,omitempty"`
	Size        int64            `json:"size"`
	Languages   []string         `json:"languages,omitempty"`
	License     manifest.License `json:"license"`
	Upstream    string           `json:"upstream"`
	Installed   bool             `json:"installed"`
	Supported   bool             `json:"supported"` // runs on this platform
	Aliases     []string         `json:"aliases,omitempty"`
	EngineError string           `json:"engine_error,omitempty"` // why the engine cannot start, if it cannot
}

func (s *Server) summarize(m *manifest.Manifest) modelSummary {
	engineErr := ""
	if _, _, err := s.runnerCommand(m); err != nil && s.cfg.Launch == nil {
		engineErr = err.Error()
	}
	return modelSummary{
		EngineError: engineErr,
		Name:        m.Ref(), Task: m.Task, Engine: m.Engine, Family: m.Family, Description: m.Description,
		Size: m.TotalSize(), Languages: m.Languages, License: m.License, Upstream: m.Upstream.Model,
		Installed: s.cfg.Store.IsInstalled(m), Supported: catalog.SupportsPlatform(m), Aliases: m.Aliases,
	}
}

// available is every model that can be run right now: downloaded models plus
// system models this platform provides.
func (s *Server) available() []*manifest.Manifest {
	out, _ := s.cfg.Store.Installed()
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.Ref()] = true
	}
	for _, m := range s.cfg.Catalog.All() {
		if m.Source.Type == manifest.SourceSystem && catalog.SupportsPlatform(m) && !seen[m.Ref()] {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	list := []modelSummary{}
	for _, m := range s.available() {
		list = append(list, s.summarize(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": list})
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	list := []modelSummary{}
	for _, m := range s.cfg.Catalog.All() {
		list = append(list, s.summarize(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": list})
}

func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	m, err := s.lookup(r.URL.Query().Get("model"))
	if err != nil {
		writeError(w, err)
		return
	}
	resp := map[string]any{
		"manifest":  m,
		"installed": s.cfg.Store.IsInstalled(m),
		"supported": catalog.SupportsPlatform(m),
	}
	if m.Source.Type != manifest.SourceSystem {
		resp["path"] = s.cfg.Store.ModelDir(m)
	}
	if _, _, err := s.runnerCommand(m); err != nil {
		resp["engine_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	m, err := s.ready(r.URL.Query().Get("model"), manifest.TaskTTS)
	if err != nil {
		writeError(w, err)
		return
	}
	voices := m.Voices
	if len(voices) == 0 {
		// The manifest does not enumerate voices (system voices vary by
		// machine), so ask the engine.
		lease, err := s.sched.Acquire(r.Context(), m, nil)
		if err != nil {
			writeError(w, err)
			return
		}
		rv, err := lease.Conn.Voices(r.Context())
		lease.Release()
		if err != nil {
			writeError(w, err)
			return
		}
		for _, v := range rv {
			voices = append(voices, manifest.Voice(v))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": m.Ref(), "default_voice": m.DefaultVoice, "voices": voices})
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errf(http.StatusBadRequest, "invalid JSON: %v", err))
		return
	}
	m, err := s.lookup(req.Model)
	if err != nil {
		writeError(w, err)
		return
	}
	// Pulling installs what the catalog says now, so pulling again is how an
	// installed model picks up a corrected manifest. A model the catalog no
	// longer lists keeps the manifest it was installed with.
	if current, err := s.cfg.Catalog.Resolve(m.Ref()); err == nil {
		m = current
	}
	if !catalog.SupportsPlatform(m) {
		writeError(w, errf(http.StatusBadRequest, "model %q does not run on this platform", m.Ref()))
		return
	}
	lock := s.pullLock(m.Ref())
	lock.Lock()
	defer lock.Unlock()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	send := func(v any) {
		enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(pull.Progress{Status: "pulling " + m.Ref(), Total: m.TotalSize()})
	if err := pull.Model(r.Context(), s.cfg.Store, m, func(p pull.Progress) { send(p) }); err != nil {
		send(map[string]string{"error": err.Error()})
	}
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errf(http.StatusBadRequest, "invalid JSON: %v", err))
		return
	}
	m, err := s.lookup(req.Model)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.sched.Unload(m.Ref()); err != nil {
		writeError(w, errf(http.StatusConflict, "%v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped", "model": m.Ref()})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	m, err := s.lookup(r.URL.Query().Get("model"))
	if err != nil {
		writeError(w, err)
		return
	}
	s.sched.Unload(m.Ref()) // best effort: fails if in use, and then so does Remove's caller
	if err := s.cfg.Store.Remove(m); err != nil {
		writeError(w, errf(http.StatusBadRequest, "%v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "model": m.Ref()})
}

// ---- OpenAI model listing ----

func (s *Server) openAIModel(m *manifest.Manifest) map[string]any {
	created := int64(0)
	if st, err := os.Stat(filepath.Join(s.cfg.Store.ModelDir(m), "manifest.json")); err == nil {
		created = st.ModTime().Unix()
	}
	return map[string]any{"id": m.Ref(), "object": "model", "created": created, "owned_by": m.Upstream.Model}
}

func (s *Server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, m := range s.available() {
		// List only what a request would actually succeed against: a model
		// whose engine is missing is downloaded but not usable.
		if _, _, err := s.runnerCommand(m); err != nil && s.cfg.Launch == nil {
			continue
		}
		data = append(data, s.openAIModel(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleOpenAIModel(w http.ResponseWriter, r *http.Request) {
	m, err := s.lookup(r.PathValue("id"))
	if err == nil && !s.cfg.Store.IsInstalled(m) {
		err = errf(http.StatusNotFound, "model %q is not installed", m.Ref())
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.openAIModel(m))
}

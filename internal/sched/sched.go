// Package sched decides which models are in memory.
//
// It follows the lifecycle Ollama made familiar: a model is loaded on first
// use, stays warm for a keep-alive period after its last request (5 minutes
// by default), and is unloaded when idle models must make room. Unloading
// ends the runner process, which returns all of its memory.
package sched

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/runner"
)

// Loaded is a running runner as the scheduler sees it.
type Loaded struct {
	Conn   *runner.Conn
	Stop   func()          // unload; must be safe to call more than once
	Exited <-chan struct{} // closed if the runner dies on its own; may be nil
	Pid    int
}

// LaunchFunc starts a runner for a model and returns once it is ready.
type LaunchFunc func(ctx context.Context, m *manifest.Manifest) (*Loaded, error)

// Forever as a keep-alive keeps a model loaded until it is stopped.
const Forever = time.Duration(-1)

// Scheduler owns every loaded model.
type Scheduler struct {
	launch    LaunchFunc
	maxLoaded int
	keepAlive time.Duration

	mu      sync.Mutex
	entries map[string]*entry
	changed chan struct{} // closed and replaced whenever state changes
	closed  bool
}

type entry struct {
	m        *manifest.Manifest
	loaded   *Loaded
	ready    chan struct{} // closed when loading finished (ok or not)
	err      error
	refs     int
	busy     chan struct{} // capacity 1: runners serve one request at a time
	loadedAt time.Time
	lastUsed time.Time
	expires  time.Time // zero when in use or kept forever
	timer    *time.Timer
	loadTook time.Duration
}

// New returns a scheduler. maxLoaded < 1 means 3; keepAlive 0 means 5 minutes.
func New(launch LaunchFunc, maxLoaded int, keepAlive time.Duration) *Scheduler {
	if maxLoaded < 1 {
		maxLoaded = 3
	}
	if keepAlive == 0 {
		keepAlive = 5 * time.Minute
	}
	return &Scheduler{
		launch:    launch,
		maxLoaded: maxLoaded,
		keepAlive: keepAlive,
		entries:   map[string]*entry{},
		changed:   make(chan struct{}),
	}
}

func (s *Scheduler) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Lease is exclusive use of a loaded model. Release it when the request ends.
type Lease struct {
	Conn *runner.Conn
	// LoadDuration is how long the model took to load if this request caused
	// the load, and zero if the model was already warm.
	LoadDuration time.Duration

	s    *Scheduler
	e    *entry
	keep *time.Duration
	once sync.Once
}

// Acquire returns a lease on the model, loading it if needed. keepAlive
// overrides the default idle period for this model when non-nil: zero unloads
// as soon as the request ends, Forever never unloads.
func (s *Scheduler) Acquire(ctx context.Context, m *manifest.Manifest, keepAlive *time.Duration) (*Lease, error) {
	key := m.Ref()
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, errors.New("server is shutting down")
		}
		e, ok := s.entries[key]
		if !ok {
			if len(s.entries) >= s.maxLoaded && !s.evictIdleLocked() {
				// Everything loaded is in use: wait for a release.
				wait := s.changed
				s.mu.Unlock()
				select {
				case <-wait:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			e = &entry{m: m, ready: make(chan struct{}), busy: make(chan struct{}, 1)}
			s.entries[key] = e
			go s.load(e)
		}
		e.refs++
		if e.timer != nil {
			e.timer.Stop()
			e.timer = nil
		}
		e.expires = time.Time{}
		s.mu.Unlock()

		caused := !ok
		select {
		case <-e.ready:
		case <-ctx.Done():
			s.release(e, nil)
			return nil, ctx.Err()
		}
		if e.err != nil {
			s.release(e, nil)
			return nil, e.err
		}
		select {
		case e.busy <- struct{}{}:
		case <-ctx.Done():
			s.release(e, nil)
			return nil, ctx.Err()
		}
		l := &Lease{Conn: e.loaded.Conn, s: s, e: e, keep: keepAlive}
		if caused {
			l.LoadDuration = e.loadTook
		}
		return l, nil
	}
}

// Release ends the lease.
func (l *Lease) Release() {
	l.once.Do(func() {
		<-l.e.busy
		l.s.release(l.e, l.keep)
	})
}

func (s *Scheduler) load(e *entry) {
	start := time.Now()
	// Loading is not tied to the first caller's context: if that caller gives
	// up, the next one should find the model warm instead of starting over.
	loaded, err := s.launch(context.Background(), e.m)
	s.mu.Lock()
	e.loaded, e.err = loaded, err
	e.loadTook = time.Since(start)
	e.loadedAt, e.lastUsed = time.Now(), time.Now()
	if err != nil {
		if s.entries[e.m.Ref()] == e {
			delete(s.entries, e.m.Ref())
		}
	}
	closed := s.closed
	s.notifyLocked()
	s.mu.Unlock()
	close(e.ready)
	if err == nil && !closed {
		// If every caller gave up while the model was loading, nobody will
		// release it; start its idle clock here so it cannot stay forever.
		s.mu.Lock()
		e.refs++
		s.mu.Unlock()
		s.release(e, nil)
	}
	if err != nil {
		return
	}
	if closed {
		loaded.Stop()
		return
	}
	if loaded.Exited != nil {
		go func() {
			<-loaded.Exited
			s.mu.Lock()
			if s.entries[e.m.Ref()] == e {
				delete(s.entries, e.m.Ref())
				if e.timer != nil {
					e.timer.Stop()
				}
				s.notifyLocked()
			}
			s.mu.Unlock()
		}()
	}
}

func (s *Scheduler) release(e *entry, keep *time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.refs--
	e.lastUsed = time.Now()
	defer s.notifyLocked()
	if e.refs > 0 || e.err != nil || s.entries[e.m.Ref()] != e {
		return
	}
	d := s.keepAlive
	if keep != nil {
		d = *keep
	}
	switch {
	case d < 0:
		e.expires = time.Time{}
	case d == 0:
		s.unloadLocked(e)
	default:
		e.expires = time.Now().Add(d)
		e.timer = time.AfterFunc(d, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if e.refs == 0 && s.entries[e.m.Ref()] == e {
				s.unloadLocked(e)
				s.notifyLocked()
			}
		})
	}
}

func (s *Scheduler) unloadLocked(e *entry) {
	delete(s.entries, e.m.Ref())
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	if e.loaded != nil {
		go e.loaded.Stop()
	}
}

// evictIdleLocked unloads the least recently used idle model.
func (s *Scheduler) evictIdleLocked() bool {
	var victim *entry
	for _, e := range s.entries {
		if e.refs != 0 || e.loaded == nil {
			continue
		}
		if victim == nil || e.lastUsed.Before(victim.lastUsed) {
			victim = e
		}
	}
	if victim == nil {
		return false
	}
	s.unloadLocked(victim)
	return true
}

// Status describes one loaded model for `oaudio ps`.
type Status struct {
	Model     string    `json:"model"`
	Task      string    `json:"task"`
	Engine    string    `json:"engine"`
	Pid       int       `json:"pid,omitempty"`
	InUse     bool      `json:"in_use"`
	Loading   bool      `json:"loading,omitempty"`
	LoadedAt  time.Time `json:"loaded_at"`
	ExpiresAt time.Time `json:"expires_at"` // zero: in use or kept forever
}

// Ps lists loaded models.
func (s *Scheduler) Ps() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.entries))
	for _, e := range s.entries {
		st := Status{Model: e.m.Ref(), Task: e.m.Task, Engine: e.m.Engine, InUse: e.refs > 0, LoadedAt: e.loadedAt, ExpiresAt: e.expires}
		if e.loaded == nil {
			st.Loading = true
		} else {
			st.Pid = e.loaded.Pid
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Unload stops a model now. It refuses while the model is serving a request.
func (s *Scheduler) Unload(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[ref]
	if !ok {
		return fmt.Errorf("%s is not loaded", ref)
	}
	if e.refs > 0 || e.loaded == nil {
		return fmt.Errorf("%s is in use", ref)
	}
	s.unloadLocked(e)
	s.notifyLocked()
	return nil
}

// Shutdown unloads everything and refuses further work.
func (s *Scheduler) Shutdown() {
	s.mu.Lock()
	s.closed = true
	var stops []func()
	for k, e := range s.entries {
		if e.timer != nil {
			e.timer.Stop()
		}
		if e.loaded != nil {
			stops = append(stops, e.loaded.Stop)
		}
		delete(s.entries, k)
	}
	s.notifyLocked()
	s.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

package sched

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/runner"
)

type fake struct {
	mu      sync.Mutex
	loads   map[string]int
	stops   map[string]int
	delay   time.Duration
	failFor string
	exits   map[string]chan struct{}
}

func newFake() *fake {
	return &fake{loads: map[string]int{}, stops: map[string]int{}, exits: map[string]chan struct{}{}}
}

func (f *fake) launch(ctx context.Context, m *manifest.Manifest) (*Loaded, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.Name == f.failFor {
		return nil, errors.New("boom")
	}
	f.loads[m.Name]++
	exit := make(chan struct{})
	f.exits[m.Name] = exit
	name := m.Name
	return &Loaded{Conn: &runner.Conn{Addr: name}, Exited: exit, Stop: func() {
		f.mu.Lock()
		f.stops[name]++
		f.mu.Unlock()
	}}, nil
}

func (f *fake) count(m map[string]int, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return m[name]
}

func model(name string) *manifest.Manifest { return &manifest.Manifest{Name: name, Tag: "t"} }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// Concurrent first requests for one model cause a single load.
func TestLoadsOnce(t *testing.T) {
	f := newFake()
	f.delay = 50 * time.Millisecond
	s := New(f.launch, 3, time.Minute)
	var wg sync.WaitGroup
	var warm atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := s.Acquire(context.Background(), model("a"), nil)
			if err != nil {
				t.Error(err)
				return
			}
			if l.LoadDuration == 0 {
				warm.Add(1)
			}
			l.Release()
		}()
	}
	wg.Wait()
	if n := f.count(f.loads, "a"); n != 1 {
		t.Errorf("loaded %d times, want 1", n)
	}
	if warm.Load() != 7 {
		t.Errorf("%d requests saw a warm model, want 7", warm.Load())
	}
}

// A runner serves one request at a time.
func TestSerializesRequests(t *testing.T) {
	s := New(newFake().launch, 3, time.Minute)
	var active, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := s.Acquire(context.Background(), model("a"), nil)
			if err != nil {
				t.Error(err)
				return
			}
			if n := active.Add(1); n > peak.Load() {
				peak.Store(n)
			}
			time.Sleep(5 * time.Millisecond)
			active.Add(-1)
			l.Release()
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Errorf("%d requests ran at once on one runner", peak.Load())
	}
}

func TestKeepAlive(t *testing.T) {
	f := newFake()
	s := New(f.launch, 3, 40*time.Millisecond)
	l, _ := s.Acquire(context.Background(), model("a"), nil)
	l.Release()
	if len(s.Ps()) != 1 || s.Ps()[0].ExpiresAt.IsZero() {
		t.Fatalf("after release: %+v", s.Ps())
	}
	// Using it again before expiry keeps it and does not reload.
	time.Sleep(20 * time.Millisecond)
	l, _ = s.Acquire(context.Background(), model("a"), nil)
	time.Sleep(60 * time.Millisecond) // longer than keep-alive, but in use
	if len(s.Ps()) != 1 || f.count(f.stops, "a") != 0 {
		t.Fatal("model unloaded while in use")
	}
	l.Release()
	eventually(t, "idle unload", func() bool { return len(s.Ps()) == 0 && f.count(f.stops, "a") == 1 })
	if f.count(f.loads, "a") != 1 {
		t.Errorf("loads = %d, want 1", f.count(f.loads, "a"))
	}

	// keep_alive 0 unloads at once; Forever never does.
	zero, forever := time.Duration(0), Forever
	l, _ = s.Acquire(context.Background(), model("b"), &zero)
	l.Release()
	eventually(t, "immediate unload", func() bool { return f.count(f.stops, "b") == 1 })
	l, _ = s.Acquire(context.Background(), model("c"), &forever)
	l.Release()
	time.Sleep(80 * time.Millisecond)
	if f.count(f.stops, "c") != 0 || !s.Ps()[0].ExpiresAt.IsZero() {
		t.Error("model kept forever was unloaded or given an expiry")
	}
}

// At the limit, the least recently used idle model makes room; if every
// model is busy, the request waits.
func TestEviction(t *testing.T) {
	f := newFake()
	s := New(f.launch, 2, time.Minute)
	for _, n := range []string{"a", "b"} {
		l, _ := s.Acquire(context.Background(), model(n), nil)
		l.Release()
		time.Sleep(2 * time.Millisecond)
	}
	la, _ := s.Acquire(context.Background(), model("a"), nil) // a is now most recent
	la.Release()
	lc, err := s.Acquire(context.Background(), model("c"), nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b evicted", func() bool { return f.count(f.stops, "b") == 1 })
	if f.count(f.stops, "a") != 0 {
		t.Error("evicted the recently used model instead of the least recently used")
	}

	la, _ = s.Acquire(context.Background(), model("a"), nil) // a and c both busy
	got := make(chan error, 1)
	go func() {
		l, err := s.Acquire(context.Background(), model("d"), nil)
		if err == nil {
			l.Release()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("request did not wait while every loaded model was busy")
	case <-time.After(40 * time.Millisecond):
	}
	lc.Release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("request never ran after a model became idle")
	}
	la.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	l1, _ := s.Acquire(context.Background(), model("a"), nil)
	l2, _ := s.Acquire(context.Background(), model("d"), nil)
	if _, err := s.Acquire(ctx, model("e"), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting request ignored its deadline: %v", err)
	}
	l1.Release()
	l2.Release()
}

func TestLoadFailureAndCrash(t *testing.T) {
	f := newFake()
	f.failFor = "bad"
	s := New(f.launch, 3, time.Minute)
	if _, err := s.Acquire(context.Background(), model("bad"), nil); err == nil {
		t.Fatal("load failure not reported")
	}
	if len(s.Ps()) != 0 {
		t.Error("failed model still listed")
	}
	l, _ := s.Acquire(context.Background(), model("a"), nil)
	l.Release()
	f.mu.Lock()
	close(f.exits["a"]) // the runner process dies
	f.mu.Unlock()
	eventually(t, "crashed model dropped", func() bool { return len(s.Ps()) == 0 })
	l, err := s.Acquire(context.Background(), model("a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	if f.count(f.loads, "a") != 2 {
		t.Errorf("loads = %d, want a reload after the crash", f.count(f.loads, "a"))
	}
}

func TestUnloadAndShutdown(t *testing.T) {
	f := newFake()
	s := New(f.launch, 3, time.Minute)
	l, _ := s.Acquire(context.Background(), model("a"), nil)
	if err := s.Unload("a:t"); err == nil {
		t.Error("unloaded a model that was in use")
	}
	l.Release()
	if err := s.Unload("a:t"); err != nil {
		t.Error(err)
	}
	l, _ = s.Acquire(context.Background(), model("b"), nil)
	l.Release()
	s.Shutdown()
	if f.count(f.stops, "b") != 1 {
		t.Error("shutdown left a model running")
	}
	if _, err := s.Acquire(context.Background(), model("c"), nil); err == nil {
		t.Error("accepted work after shutdown")
	}
}

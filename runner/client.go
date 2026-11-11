package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// Conn is a connection to a running runner, in-process or not.
type Conn struct {
	Addr string
	Info Info
	http *http.Client
}

// Dial connects to a runner that is already listening and fetches its Info.
func Dial(ctx context.Context, addr string) (*Conn, error) {
	c := &Conn{Addr: addr, http: &http.Client{}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/info", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runner info: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&c.Info); err != nil {
		return nil, err
	}
	if c.Info.Protocol != ProtocolVersion {
		return nil, fmt.Errorf("runner speaks protocol %d, this daemon speaks %d", c.Info.Protocol, ProtocolVersion)
	}
	return c, nil
}

// Error is an error reported by a runner, with the HTTP status it chose.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func decodeError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if json.Unmarshal(data, &body) != nil || body.Error == "" {
		body.Error = fmt.Sprintf("runner returned %s", resp.Status)
	}
	return &Error{Status: resp.StatusCode, Message: body.Error}
}

// Voices lists the speakers of a synthesis runner.
func (c *Conn) Voices(ctx context.Context) ([]Voice, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.Addr+"/v1/voices", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var v []Voice
	return v, json.NewDecoder(resp.Body).Decode(&v)
}

// Synthesize starts synthesis and returns the PCM stream and its sample rate.
// The caller must close the stream.
func (c *Conn) Synthesize(ctx context.Context, sreq SynthesisRequest) (pcm io.ReadCloser, sampleRate int, err error) {
	body, _ := json.Marshal(sreq)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.Addr+"/v1/synthesize", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, decodeError(resp)
	}
	sampleRate, _ = strconv.Atoi(resp.Header.Get("X-Sample-Rate"))
	if sampleRate <= 0 {
		resp.Body.Close()
		return nil, 0, errors.New("runner did not report a sample rate")
	}
	return resp.Body, sampleRate, nil
}

// Transcribe sends mono s16le PCM and calls onSegment for each segment as the
// runner produces it. It returns the detected language.
func (c *Conn) Transcribe(ctx context.Context, pcm []byte, opts TranscribeOptions, onSegment func(Segment) error) (language string, err error) {
	q := url.Values{}
	q.Set("sample_rate", strconv.Itoa(opts.SampleRate))
	if opts.Language != "" {
		q.Set("language", opts.Language)
	}
	if opts.Task != "" {
		q.Set("task", opts.Task)
	}
	if opts.Prompt != "" {
		q.Set("prompt", opts.Prompt)
	}
	if opts.Temperature != 0 {
		q.Set("temperature", strconv.FormatFloat(opts.Temperature, 'g', -1, 64))
	}
	if opts.WordTimestamps {
		q.Set("word_timestamps", "1")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.Addr+"/v1/transcribe?"+q.Encode(), bytes.NewReader(pcm))
	req.Header.Set("Content-Type", "audio/pcm")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", decodeError(resp)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return "", errors.New("runner closed the stream before finishing")
			}
			return "", err
		}
		switch ev.Type {
		case EventSegment:
			if ev.Segment != nil {
				if err := onSegment(*ev.Segment); err != nil {
					return "", err
				}
			}
		case EventDone:
			return ev.Language, nil
		case EventError:
			return "", &Error{Status: http.StatusInternalServerError, Message: ev.Message}
		}
	}
}

func (c *Conn) postPCM(ctx context.Context, path string, q url.Values, pcm []byte, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.Addr+path+"?"+q.Encode(), bytes.NewReader(pcm))
	req.Header.Set("Content-Type", "audio/pcm")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Detect sends mono s16le PCM and returns the spans of speech in it.
func (c *Conn) Detect(ctx context.Context, pcm []byte, opts DetectOptions) ([]Span, error) {
	q := url.Values{"sample_rate": {strconv.Itoa(opts.SampleRate)}}
	if opts.Threshold > 0 {
		q.Set("threshold", strconv.FormatFloat(opts.Threshold, 'g', -1, 64))
	}
	if opts.MinSilenceMS > 0 {
		q.Set("min_silence_ms", strconv.Itoa(opts.MinSilenceMS))
	}
	if opts.MinSpeechMS > 0 {
		q.Set("min_speech_ms", strconv.Itoa(opts.MinSpeechMS))
	}
	var out struct {
		Spans []Span `json:"spans"`
	}
	return out.Spans, c.postPCM(ctx, "/v1/vad", q, pcm, &out)
}

// Diarize sends mono s16le PCM and returns speaker turns.
func (c *Conn) Diarize(ctx context.Context, pcm []byte, opts DiarizeOptions) ([]Turn, error) {
	q := url.Values{"sample_rate": {strconv.Itoa(opts.SampleRate)}}
	if opts.NumSpeakers > 0 {
		q.Set("num_speakers", strconv.Itoa(opts.NumSpeakers))
	}
	if opts.Threshold > 0 {
		q.Set("threshold", strconv.FormatFloat(opts.Threshold, 'g', -1, 64))
	}
	var out struct {
		Turns []Turn `json:"turns"`
	}
	return out.Turns, c.postPCM(ctx, "/v1/diarize", q, pcm, &out)
}

// Process is a runner started as a child process.
type Process struct {
	*Conn
	cmd    *exec.Cmd
	stdin  io.Closer
	exited chan struct{}
	once   sync.Once
	werr   error
}

// Launch starts exe with args, waits for its handshake (which a runner prints
// only after its model is loaded), and connects. logs receives the runner's
// stderr. The runner exits when Stop is called or this process dies, because
// it watches the stdin pipe held here.
func Launch(ctx context.Context, exe string, args []string, logs io.Writer, loadTimeout time.Duration) (*Process, error) {
	cmd := exec.Command(exe, args...)
	cmd.Stderr = logs
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start runner: %w", err)
	}
	p := &Process{cmd: cmd, stdin: stdin, exited: make(chan struct{})}

	type result struct {
		hs  Handshake
		err error
	}
	hsc := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		var r result
		if sc.Scan() {
			r.err = json.Unmarshal(sc.Bytes(), &r.hs)
			if r.err == nil && (r.hs.Protocol == 0 || r.hs.Addr == "") {
				r.err = fmt.Errorf("unexpected runner output %q", sc.Text())
			}
		} else {
			r.err = errors.New("runner exited before it was ready (see the server log for its error)")
		}
		hsc <- r
		io.Copy(io.Discard, stdout) // keep draining so the runner never blocks on stdout
	}()
	go func() {
		p.werr = cmd.Wait()
		close(p.exited)
	}()

	timer := time.NewTimer(loadTimeout)
	defer timer.Stop()
	select {
	case r := <-hsc:
		if r.err != nil {
			p.Stop()
			return nil, r.err
		}
		if r.hs.Protocol != ProtocolVersion {
			p.Stop()
			return nil, fmt.Errorf("runner speaks protocol %d, this daemon speaks %d", r.hs.Protocol, ProtocolVersion)
		}
		conn, err := Dial(ctx, r.hs.Addr)
		if err != nil {
			p.Stop()
			return nil, err
		}
		p.Conn = conn
		return p, nil
	case <-timer.C:
		p.Stop()
		return nil, fmt.Errorf("runner did not become ready within %s", loadTimeout)
	case <-ctx.Done():
		p.Stop()
		return nil, ctx.Err()
	}
}

// Exited is closed when the runner process has ended.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// Pid returns the runner's process id.
func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Stop asks the runner to exit by closing its stdin and kills it if it has
// not gone within three seconds. All memory the model held is returned to the
// system when the process ends.
func (p *Process) Stop() {
	p.once.Do(func() {
		p.stdin.Close()
		select {
		case <-p.exited:
		case <-time.After(3 * time.Second):
			if p.cmd.Process != nil {
				p.cmd.Process.Kill()
			}
			<-p.exited
		}
	})
}

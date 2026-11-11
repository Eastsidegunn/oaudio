package runner

import (
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Limits on streams a runner keeps between requests. The daemon ends every
// stream it starts, so these only matter when it cannot (it was killed, or a
// client vanished mid-utterance).
const (
	streamIdle = 2 * time.Minute // a stream not touched for this long is dropped
	maxStreams = 16              // beyond this the least recently used is dropped
)

type openStream struct {
	s    Stream
	used time.Time
}

// streamHandlers adds POST and DELETE /v1/stream to mux. mu is the lock that
// keeps the engine's calls one at a time.
func streamHandlers(mux *http.ServeMux, engine Engine, mu *sync.Mutex) {
	streams := map[string]*openStream{}
	// drop closes streams left behind; called with mu held.
	drop := func(now time.Time) {
		for id, o := range streams {
			if now.Sub(o.used) > streamIdle {
				o.s.Close()
				delete(streams, id)
			}
		}
		for len(streams) >= maxStreams {
			oldest := ""
			for id, o := range streams {
				if oldest == "" || o.used.Before(streams[oldest].used) {
					oldest = id
				}
			}
			streams[oldest].s.Close()
			delete(streams, oldest)
		}
	}

	mux.HandleFunc("POST /v1/stream", func(w http.ResponseWriter, r *http.Request) {
		st, ok := engine.(Streamer)
		if !ok {
			httpError(w, http.StatusNotImplemented, "this runner does not recognize speech as it arrives")
			return
		}
		q := r.URL.Query()
		opts := StreamOptions{Session: q.Get("session"), Language: q.Get("language"), Start: q.Get("start") == "1", End: q.Get("end") == "1"}
		opts.SampleRate, _ = strconv.Atoi(q.Get("sample_rate"))
		if opts.Session == "" || opts.SampleRate <= 0 {
			httpError(w, http.StatusBadRequest, "session and sample_rate are required")
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
		now := time.Now()
		o := streams[opts.Session]
		if o != nil && opts.Start {
			o.s.Close() // a session id reused: the old stream is over
			delete(streams, opts.Session)
			o = nil
		}
		if o == nil {
			if !opts.Start {
				// Dropped as idle, or the runner restarted: the audio so far
				// is gone, and a transcript of the rest alone would mislead.
				httpError(w, http.StatusNotFound, "unknown stream "+strconv.Quote(opts.Session))
				return
			}
			drop(now)
			s, err := st.OpenStream(opts)
			if err != nil {
				streamFail(w, err)
				return
			}
			o = &openStream{s: s}
			streams[opts.Session] = o
		}
		o.used = now
		res := StreamResult{}
		if len(samples) > 0 || !opts.End {
			if res.Text, err = o.s.Accept(samples); err != nil {
				o.s.Close()
				delete(streams, opts.Session)
				streamFail(w, err)
				return
			}
		}
		if opts.End {
			res.Text, res.Language, err = o.s.Finish()
			o.s.Close()
			delete(streams, opts.Session)
			if err != nil {
				streamFail(w, err)
				return
			}
			res.Done = true
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("DELETE /v1/stream", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("session")
		mu.Lock()
		if o := streams[id]; o != nil {
			o.s.Close()
			delete(streams, id)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
}

func streamFail(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var bad *BadRequestError
	if errors.As(err, &bad) {
		status = http.StatusBadRequest
	}
	httpError(w, status, err.Error())
}

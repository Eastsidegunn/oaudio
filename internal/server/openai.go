package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eastsidegunn/oaudio/internal/audio"
	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/runner"
)

// openAIPCMRate is the sample rate OpenAI documents for response_format=pcm.
// Clients written against OpenAI assume it, so pcm is always delivered at
// this rate regardless of the model's native rate.
const openAIPCMRate = 24000

// ---- POST /v1/audio/speech ----

type speechRequest struct {
	Model          string          `json:"model"`
	Input          string          `json:"input"`
	Voice          json.RawMessage `json:"voice"` // string, or {"id": "..."}
	ResponseFormat string          `json:"response_format"`
	Speed          *float64        `json:"speed"`
	StreamFormat   string          `json:"stream_format"`
	Instructions   string          `json:"instructions"`

	// Extensions.
	Language  string          `json:"language"`
	KeepAlive json.RawMessage `json:"keep_alive"`
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.ID != "" {
		return obj.ID
	}
	return strings.Trim(string(raw), `"`)
}

func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req speechRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeError(w, errf(http.StatusBadRequest, "invalid JSON body: %v", err))
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeError(w, errf(http.StatusBadRequest, "input is required"))
		return
	}
	m, err := s.ready(req.Model, manifest.TaskTTS)
	if err != nil {
		writeError(w, err)
		return
	}
	speed := 1.0
	if req.Speed != nil {
		speed = *req.Speed
		if speed < 0.25 || speed > 4 {
			writeError(w, errf(http.StatusBadRequest, "speed must be between 0.25 and 4.0"))
			return
		}
		if speed != 1 && !m.Has(manifest.CapSpeed) {
			writeError(w, errf(http.StatusBadRequest, "model %q does not support speed", m.Ref()))
			return
		}
	}
	// OpenAI's default is mp3. Without an encoder available the default
	// becomes wav; asking for mp3 explicitly then fails loudly instead.
	formatName := req.ResponseFormat
	if formatName == "" {
		formatName = "mp3"
		if audio.FFmpeg() == "" {
			formatName = "wav"
		}
	}
	format, ok := audio.LookupFormat(formatName)
	if !ok {
		writeError(w, errf(http.StatusBadRequest, "response_format %q is not supported; use mp3, opus, aac, flac, wav or pcm", formatName))
		return
	}
	if !format.Native() && audio.FFmpeg() == "" {
		writeError(w, errf(http.StatusBadRequest, "response_format %q needs ffmpeg, which was not found on PATH; install ffmpeg or use wav or pcm", formatName))
		return
	}
	sse := false
	switch req.StreamFormat {
	case "", "audio":
	case "sse":
		sse = true
	default:
		writeError(w, errf(http.StatusBadRequest, "stream_format must be audio or sse"))
		return
	}
	keep, err := ParseKeepAlive(strings.Trim(string(req.KeepAlive), `"`))
	if err != nil {
		writeError(w, errf(http.StatusBadRequest, "%v", err))
		return
	}

	// A voice may be a custom one made from a recording, by id or by name.
	voice := rawString(req.Voice)
	var reference *runner.Reference
	if voice != "" {
		custom, err := s.voices.Get(voice)
		if err != nil {
			writeError(w, err)
			return
		}
		if custom != nil {
			if !m.Has(manifest.CapVoiceClone) {
				writeError(w, errf(http.StatusBadRequest, "%q is a custom voice, and model %q cannot clone voices; use a model with the voice_clone capability", custom.Name, m.Ref()))
				return
			}
			reference = &runner.Reference{AudioPath: s.voices.ReferencePath(custom), Text: custom.Transcript}
			voice = ""
		}
	}

	lease, err := s.sched.Acquire(r.Context(), m, keep)
	if err != nil {
		writeError(w, err)
		return
	}
	defer lease.Release()

	chunks := SplitSentences(req.Input)
	if lease.Conn.Info.WholeInput {
		chunks = []string{strings.TrimSpace(req.Input)}
	}
	var (
		enc        audio.Encoder
		sink       io.Writer
		firstAudio time.Duration
		pcmBytes   int64
		outRate    int
	)
	for i, text := range chunks {
		stream, rate, err := lease.Conn.Synthesize(r.Context(), runner.SynthesisRequest{
			Text: text, Voice: voice, Language: req.Language, Speed: speed, Reference: reference,
		})
		if err != nil {
			if i == 0 {
				writeError(w, err)
			} else {
				s.log.Error("synthesis failed mid-stream", "model", m.Ref(), "err", err)
				panic(http.ErrAbortHandler) // truncate rather than end cleanly
			}
			return
		}
		if i == 0 {
			outRate = rate
			if format.Name == "pcm" {
				outRate = openAIPCMRate
			}
			h := w.Header()
			if sse {
				h.Set("Content-Type", "text/event-stream")
				h.Set("Cache-Control", "no-cache")
			} else if format.Name == "pcm" {
				h.Set("Content-Type", fmt.Sprintf("audio/pcm;rate=%d", outRate))
			} else {
				h.Set("Content-Type", format.ContentType)
			}
			h.Set("X-Oaudio-Model", m.Ref())
			h.Set("X-Oaudio-Sample-Rate", strconv.Itoa(outRate))
			if req.Instructions != "" {
				h.Set("X-Oaudio-Warning", "instructions are ignored by this model")
			}
			if lease.LoadDuration > 0 {
				h.Set("Server-Timing", fmt.Sprintf("load;dur=%d", lease.LoadDuration.Milliseconds()))
			}
			w.WriteHeader(http.StatusOK)
			sink = &flushWriter{w: w}
			if sse {
				sink = &sseAudioWriter{w: sink}
			}
			enc, err = audio.NewEncoder(r.Context(), format, outRate, sink)
			if err != nil {
				stream.Close()
				s.log.Error("encoder failed", "err", err)
				panic(http.ErrAbortHandler)
			}
		}
		n, err := copyPCM(enc, stream, rate, outRate, func() {
			if firstAudio == 0 {
				firstAudio = time.Since(start)
			}
		})
		stream.Close()
		pcmBytes += n
		if err != nil {
			s.log.Error("synthesis stream failed", "model", m.Ref(), "err", err)
			enc.Close()
			panic(http.ErrAbortHandler)
		}
	}
	if err := enc.Close(); err != nil {
		s.log.Error("encoder failed", "err", err)
		panic(http.ErrAbortHandler)
	}
	if sse {
		fmt.Fprintf(sink.(*sseAudioWriter).w, "data: {\"type\":\"speech.audio.done\"}\n\n")
	}
	audioSec := float64(pcmBytes) / 2 / float64(outRate)
	s.log.Info("speech", "model", m.Ref(), "chars", utf8.RuneCountInString(req.Input), "chunks", len(chunks),
		"audio_s", fmt.Sprintf("%.2f", audioSec), "first_audio", firstAudio.Round(time.Millisecond),
		"total", time.Since(start).Round(time.Millisecond), "load", lease.LoadDuration.Round(time.Millisecond))
}

// copyPCM moves s16le PCM from a runner to an encoder. If the rates differ
// the chunk is resampled as a whole, which is fine because the daemon already
// feeds runners one sentence at a time.
func copyPCM(dst io.Writer, src io.Reader, inRate, outRate int, onFirst func()) (int64, error) {
	if inRate != outRate {
		raw, err := io.ReadAll(src)
		if err != nil {
			return 0, err
		}
		raw = raw[:len(raw)&^1]
		out := audio.FloatToS16(audio.Resample(audio.S16ToFloat(raw), inRate, outRate))
		if len(out) > 0 {
			onFirst()
		}
		n, err := dst.Write(out)
		return int64(n), err
	}
	var total int64
	buf := make([]byte, 16*1024)
	var carry []byte // keep samples whole across reads
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			onFirst()
			data := append(carry, buf[:n]...)
			even := len(data) &^ 1
			if _, werr := dst.Write(data[:even]); werr != nil {
				return total, werr
			}
			total += int64(even)
			carry = append([]byte(nil), data[even:]...)
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

type flushWriter struct{ w http.ResponseWriter }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// sseAudioWriter wraps encoded audio in OpenAI's speech.audio.delta events.
type sseAudioWriter struct{ w io.Writer }

func (s *sseAudioWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	ev, _ := json.Marshal(map[string]string{"type": "speech.audio.delta", "audio": base64.StdEncoding.EncodeToString(p)})
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", ev); err != nil {
		return 0, err
	}
	return len(p), nil
}

// SplitSentences cuts text into pieces a runner synthesizes one at a time, so
// the first audio is ready after one sentence rather than after the whole
// input. Pieces longer than maxChunk runes are cut at the last clause break
// or space.
func SplitSentences(text string) []string {
	const maxChunk = 400
	var out []string
	var cur []rune
	flush := func() {
		if s := strings.TrimSpace(string(cur)); s != "" {
			out = append(out, s)
		}
		cur = cur[:0]
	}
	runes := []rune(text)
	for i, r := range runes {
		cur = append(cur, r)
		next := rune(' ')
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		switch {
		case r == '\n':
			flush()
		case strings.ContainsRune("。！？", r):
			flush()
		case strings.ContainsRune(".!?…", r) && (unicode.IsSpace(next) || i+1 == len(runes)):
			// "3.14" and "e.g.x" are not sentence ends; a following space is.
			flush()
		case len(cur) >= maxChunk:
			cut := -1
			for j := len(cur) - 1; j > maxChunk/2; j-- {
				if strings.ContainsRune(",;:、，", cur[j]) {
					cut = j + 1
					break
				}
			}
			if cut < 0 {
				for j := len(cur) - 1; j > maxChunk/2; j-- {
					if unicode.IsSpace(cur[j]) {
						cut = j + 1
						break
					}
				}
			}
			if cut < 0 {
				cut = len(cur)
			}
			rest := append([]rune(nil), cur[cut:]...)
			cur = cur[:cut]
			flush()
			cur = append(cur, rest...)
		}
	}
	flush()
	return out
}

// ---- POST /v1/audio/transcriptions, /v1/audio/translations ----

func (s *Server) handleTranscription(translate bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeError(w, errf(http.StatusBadRequest, "expected multipart/form-data with a file field: %v", err))
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("file")
		if err != nil {
			writeError(w, errf(http.StatusBadRequest, "file is required"))
			return
		}
		data, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			writeError(w, errf(http.StatusBadRequest, "could not read file: %v", err))
			return
		}
		m, err := s.ready(r.FormValue("model"), manifest.TaskSTT)
		if err != nil {
			writeError(w, err)
			return
		}
		opts := runner.TranscribeOptions{
			SampleRate: m.Audio.SampleRate,
			Language:   strings.ToLower(strings.TrimSpace(r.FormValue("language"))),
			Prompt:     r.FormValue("prompt"),
			Task:       "transcribe",
		}
		if translate {
			if !m.Has(manifest.CapTranslate) {
				writeError(w, errf(http.StatusBadRequest, "model %q does not translate", m.Ref()))
				return
			}
			opts.Task, opts.Language = "translate", ""
		}
		if opts.Prompt != "" && !m.Has(manifest.CapPrompt) {
			writeError(w, errf(http.StatusBadRequest, "model %q does not accept a prompt", m.Ref()))
			return
		}
		if t := r.FormValue("temperature"); t != "" {
			if opts.Temperature, err = strconv.ParseFloat(t, 64); err != nil || opts.Temperature < 0 || opts.Temperature > 1 {
				writeError(w, errf(http.StatusBadRequest, "temperature must be between 0 and 1"))
				return
			}
		}
		format := r.FormValue("response_format")
		if format == "" {
			format = "json"
		}
		switch format {
		case "json", "text", "srt", "verbose_json", "vtt", "diarized_json":
		default:
			writeError(w, errf(http.StatusBadRequest, "response_format %q is not supported; use json, text, srt, verbose_json, vtt or diarized_json", format))
			return
		}
		// Speaker labels come from a separate diarization model, named in the
		// diarization_model field or, if only one is installed, implied.
		var diarizer *manifest.Manifest
		var diarOpts runner.DiarizeOptions
		if format == "diarized_json" {
			if translate {
				writeError(w, errf(http.StatusBadRequest, "diarized_json is only available for transcriptions"))
				return
			}
			if len(r.Form["known_speaker_names[]"])+len(r.Form["known_speaker_names"]) > 0 {
				writeError(w, errf(http.StatusBadRequest, "known_speaker_names is not supported; speakers are labelled A, B, ..."))
				return
			}
			if diarizer, err = s.modelFor(r.FormValue("diarization_model"), manifest.TaskDiarize); err != nil {
				writeError(w, err)
				return
			}
			if diarOpts, err = diarizeOptions(r); err != nil {
				writeError(w, err)
				return
			}
		}
		grans := append(r.Form["timestamp_granularities[]"], r.Form["timestamp_granularities"]...)
		wantWords := false
		for _, g := range grans {
			switch g {
			case "word":
				wantWords = true
			case "segment":
			default:
				writeError(w, errf(http.StatusBadRequest, "timestamp_granularities must be word or segment"))
				return
			}
		}
		if len(grans) > 0 && format != "verbose_json" {
			writeError(w, errf(http.StatusBadRequest, "timestamp_granularities requires response_format=verbose_json"))
			return
		}
		if wantWords && !m.Has(manifest.CapWordTimestamps) {
			writeError(w, errf(http.StatusBadRequest, "model %q does not produce word timestamps", m.Ref()))
			return
		}
		if (format == "srt" || format == "vtt" || format == "verbose_json" || format == "diarized_json") && !m.Has(manifest.CapSegmentTimestamps) {
			writeError(w, errf(http.StatusBadRequest, "model %q does not produce timestamps, so %s is not available", m.Ref(), format))
			return
		}
		// Word times let a segment that spans a change of speaker be split there.
		opts.WordTimestamps = wantWords || (diarizer != nil && m.Has(manifest.CapWordTimestamps))
		stream := r.FormValue("stream") == "true"
		if stream && format != "json" {
			writeError(w, errf(http.StatusBadRequest, "stream requires response_format=json"))
			return
		}
		keep, err := ParseKeepAlive(r.FormValue("keep_alive"))
		if err != nil {
			writeError(w, errf(http.StatusBadRequest, "%v", err))
			return
		}

		samples, err := audio.Decode(r.Context(), data, m.Audio.SampleRate)
		if err != nil {
			writeError(w, errf(http.StatusBadRequest, "%v", err))
			return
		}
		duration := float64(len(samples)) / float64(m.Audio.SampleRate)
		pcm := audio.FloatToS16(samples)
		decodeTook := time.Since(start)

		lease, err := s.sched.Acquire(r.Context(), m, keep)
		if err != nil {
			writeError(w, err)
			return
		}
		defer lease.Release()

		var (
			segments []runner.Segment
			text     string
			sseOpen  bool
			flusher  http.Flusher
		)
		inferStart := time.Now()
		lang, err := lease.Conn.Transcribe(r.Context(), pcm, opts, func(seg runner.Segment) error {
			prev := text
			text = JoinText(text, seg.Text)
			segments = append(segments, seg)
			if stream {
				if !sseOpen {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					w.WriteHeader(http.StatusOK)
					flusher, _ = w.(http.Flusher)
					sseOpen = true
				}
				ev, _ := json.Marshal(map[string]string{"type": "transcript.text.delta", "delta": text[len(prev):]})
				fmt.Fprintf(w, "data: %s\n\n", ev)
				if flusher != nil {
					flusher.Flush()
				}
			}
			return nil
		})
		if err != nil {
			if sseOpen {
				s.log.Error("transcription failed mid-stream", "model", m.Ref(), "err", err)
				panic(http.ErrAbortHandler)
			}
			writeError(w, err)
			return
		}
		infer := time.Since(inferStart)
		if opts.Language != "" && lang == "" {
			lang = opts.Language
		}
		s.log.Info("transcription", "model", m.Ref(), "audio_s", fmt.Sprintf("%.2f", duration), "segments", len(segments),
			"decode", decodeTook.Round(time.Millisecond), "infer", infer.Round(time.Millisecond),
			"rtf", fmt.Sprintf("%.4f", infer.Seconds()/maxf(duration, 0.001)), "load", lease.LoadDuration.Round(time.Millisecond))

		w.Header().Set("X-Oaudio-Model", m.Ref())
		switch {
		case stream:
			if !sseOpen {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
			}
			ev, _ := json.Marshal(map[string]string{"type": "transcript.text.done", "text": text})
			fmt.Fprintf(w, "data: %s\n\n", ev)
		case format == "json":
			writeJSON(w, http.StatusOK, map[string]string{"text": text})
		case format == "text":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, text)
		case format == "srt" || format == "vtt":
			cues := make([]audio.Cue, len(segments))
			for i, seg := range segments {
				cues[i] = audio.Cue{Start: seg.Start, End: seg.End, Text: seg.Text}
			}
			if format == "srt" {
				w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
				io.WriteString(w, audio.SRT(cues))
			} else {
				w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
				io.WriteString(w, audio.VTT(cues))
			}
		case format == "verbose_json":
			writeJSON(w, http.StatusOK, verboseJSON(opts.Task, lang, duration, text, segments, wantWords, opts.Temperature))
		case format == "diarized_json":
			lease.Release() // the transcription model is done; do not hold it while diarizing
			turns, err := s.diarize(r.Context(), diarizer, samples, m.Audio.SampleRate, diarOpts)
			if err != nil {
				writeError(w, err)
				return
			}
			w.Header().Set("X-Oaudio-Diarization-Model", diarizer.Ref())
			writeJSON(w, http.StatusOK, diarizedJSON(duration, text, AssignSpeakers(segments, turns)))
		}
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// verboseJSON builds OpenAI's verbose transcription object. Fields an engine
// does not report (tokens, log-probabilities) are present with zero values so
// typed clients can still decode the object.
func verboseJSON(task, lang string, duration float64, text string, segs []runner.Segment, words bool, temp float64) map[string]any {
	out := map[string]any{
		"task":     task,
		"language": languageName(lang),
		"duration": duration,
		"text":     text,
	}
	list := make([]map[string]any, len(segs))
	var allWords []runner.Word
	for i, seg := range segs {
		list[i] = map[string]any{
			"id": i, "seek": 0, "start": seg.Start, "end": seg.End, "text": seg.Text,
			"tokens": []int{}, "temperature": temp, "avg_logprob": 0.0, "compression_ratio": 0.0, "no_speech_prob": 0.0,
		}
		allWords = append(allWords, seg.Words...)
	}
	out["segments"] = list
	if words {
		if allWords == nil {
			allWords = []runner.Word{}
		}
		out["words"] = allWords
	}
	return out
}

// OpenAI's verbose_json reports the language as an English name.
var languageNames = map[string]string{
	"en": "english", "ko": "korean", "ja": "japanese", "zh": "chinese", "es": "spanish", "fr": "french",
	"de": "german", "it": "italian", "pt": "portuguese", "ru": "russian", "nl": "dutch", "ar": "arabic",
	"hi": "hindi", "tr": "turkish", "pl": "polish", "vi": "vietnamese", "id": "indonesian", "th": "thai",
	"uk": "ukrainian", "sv": "swedish", "yue": "cantonese",
}

func languageName(code string) string {
	if n, ok := languageNames[code]; ok {
		return n
	}
	return code
}

// JoinText appends a segment to running text. A space is inserted unless one
// is already there or the join is inside text written without spaces
// (Chinese, Japanese).
func JoinText(text, seg string) string {
	seg = strings.TrimRight(seg, " \t\n")
	if text == "" {
		return strings.TrimLeft(seg, " \t\n")
	}
	if seg == "" {
		return text
	}
	last, _ := utf8.DecodeLastRuneInString(text)
	first, _ := utf8.DecodeRuneInString(seg)
	switch {
	case unicode.IsSpace(last), unicode.IsSpace(first), unspaced(last):
		return text + seg
	case unspaced(first) && (unicode.IsLetter(last) || unicode.IsDigit(last)):
		// A Latin word inside a Japanese or Chinese sentence: "Python" + "は".
		return text + seg
	}
	return text + " " + seg
}

func unspaced(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) || bytes.ContainsRune([]byte("。、！？，"), r)
}

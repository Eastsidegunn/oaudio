package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/eastsidegunn/oaudio/internal/audio"
	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/internal/voices"
	"github.com/eastsidegunn/oaudio/runner"
)

// upload reads the multipart field holding an audio file.
func upload(w http.ResponseWriter, r *http.Request, field string) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return nil, errf(http.StatusBadRequest, "expected multipart/form-data with a %s field: %v", field, err)
	}
	file, _, err := r.FormFile(field)
	if err != nil {
		return nil, errf(http.StatusBadRequest, "%s is required", field)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, errf(http.StatusBadRequest, "could not read %s: %v", field, err)
	}
	return data, nil
}

// defaultModel picks the model for a task when a request does not name one:
// the only usable model of that task, if there is exactly one.
func (s *Server) defaultModel(task string) (*manifest.Manifest, error) {
	var found []*manifest.Manifest
	for _, m := range s.available() {
		if m.Task != task {
			continue
		}
		if _, _, err := s.runnerCommand(m); err != nil && s.cfg.Launch == nil {
			continue
		}
		found = append(found, m)
	}
	what := map[string]string{manifest.TaskVAD: "voice activity detection", manifest.TaskDiarize: "diarization", manifest.TaskSTT: "transcription", manifest.TaskTTS: "speech"}[task]
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, errf(http.StatusBadRequest, "no %s model is installed; run `oaudio catalog` to find one", what)
	}
	names := make([]string, len(found))
	for i, m := range found {
		names[i] = m.Ref()
	}
	return nil, errf(http.StatusBadRequest, "several %s models are installed (%s); name the one to use", what, strings.Join(names, ", "))
}

// modelFor resolves a named model, or the default for the task when name is empty.
func (s *Server) modelFor(name, task string) (*manifest.Manifest, error) {
	if strings.TrimSpace(name) == "" {
		return s.defaultModel(task)
	}
	return s.ready(name, task)
}

// ---- POST /v1/audio/vad ----

func (s *Server) detect(ctx context.Context, m *manifest.Manifest, samples []float32, rate int, opts runner.DetectOptions) ([]runner.Span, error) {
	lease, err := s.sched.Acquire(ctx, m, nil)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	opts.SampleRate = m.Audio.SampleRate
	return lease.Conn.Detect(ctx, audio.FloatToS16(audio.Resample(samples, rate, m.Audio.SampleRate)), opts)
}

func (s *Server) handleVAD(w http.ResponseWriter, r *http.Request) {
	data, err := upload(w, r, "file")
	if err != nil {
		writeError(w, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	m, err := s.modelFor(r.FormValue("model"), manifest.TaskVAD)
	if err != nil {
		writeError(w, err)
		return
	}
	var opts runner.DetectOptions
	if v := r.FormValue("threshold"); v != "" {
		if opts.Threshold, err = strconv.ParseFloat(v, 64); err != nil || opts.Threshold <= 0 || opts.Threshold >= 1 {
			writeError(w, errf(http.StatusBadRequest, "threshold must be between 0 and 1"))
			return
		}
	}
	for field, dst := range map[string]*int{"min_silence_ms": &opts.MinSilenceMS, "min_speech_ms": &opts.MinSpeechMS} {
		if v := r.FormValue(field); v != "" {
			if *dst, err = strconv.Atoi(v); err != nil || *dst <= 0 {
				writeError(w, errf(http.StatusBadRequest, "%s must be a positive number of milliseconds", field))
				return
			}
		}
	}
	samples, err := audio.Decode(r.Context(), data, m.Audio.SampleRate)
	if err != nil {
		writeError(w, errf(http.StatusBadRequest, "%v", err))
		return
	}
	spans, err := s.detect(r.Context(), m, samples, m.Audio.SampleRate, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	speech := 0.0
	for _, sp := range spans {
		speech += sp.End - sp.Start
	}
	if spans == nil {
		spans = []runner.Span{}
	}
	w.Header().Set("X-Oaudio-Model", m.Ref())
	writeJSON(w, http.StatusOK, map[string]any{
		"duration":        float64(len(samples)) / float64(m.Audio.SampleRate),
		"speech_duration": speech,
		"segments":        spans,
	})
}

// ---- POST /v1/audio/diarizations ----

// SpeakerLabel names speakers the way OpenAI's diarized transcripts do:
// A, B, ... Z, then AA, AB, ...
func SpeakerLabel(i int) string {
	label := ""
	for i >= 0 {
		label = string(rune('A'+i%26)) + label
		i = i/26 - 1
	}
	return label
}

func (s *Server) diarize(ctx context.Context, m *manifest.Manifest, samples []float32, rate int, opts runner.DiarizeOptions) ([]runner.Turn, error) {
	lease, err := s.sched.Acquire(ctx, m, nil)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	opts.SampleRate = m.Audio.SampleRate
	turns, err := lease.Conn.Diarize(ctx, audio.FloatToS16(audio.Resample(samples, rate, m.Audio.SampleRate)), opts)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].Start < turns[j].Start })
	return turns, nil
}

func diarizeOptions(r *http.Request) (runner.DiarizeOptions, error) {
	var opts runner.DiarizeOptions
	var err error
	if v := r.FormValue("num_speakers"); v != "" {
		if opts.NumSpeakers, err = strconv.Atoi(v); err != nil || opts.NumSpeakers < 1 {
			return opts, errf(http.StatusBadRequest, "num_speakers must be a positive whole number")
		}
	}
	if v := r.FormValue("speaker_threshold"); v != "" {
		if opts.Threshold, err = strconv.ParseFloat(v, 64); err != nil || opts.Threshold <= 0 {
			return opts, errf(http.StatusBadRequest, "speaker_threshold must be a positive number")
		}
	}
	return opts, nil
}

func (s *Server) handleDiarize(w http.ResponseWriter, r *http.Request) {
	data, err := upload(w, r, "file")
	if err != nil {
		writeError(w, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	m, err := s.modelFor(r.FormValue("model"), manifest.TaskDiarize)
	if err != nil {
		writeError(w, err)
		return
	}
	opts, err := diarizeOptions(r)
	if err != nil {
		writeError(w, err)
		return
	}
	samples, err := audio.Decode(r.Context(), data, m.Audio.SampleRate)
	if err != nil {
		writeError(w, errf(http.StatusBadRequest, "%v", err))
		return
	}
	turns, err := s.diarize(r.Context(), m, samples, m.Audio.SampleRate, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	speakers := []string{}
	segments := make([]map[string]any, len(turns))
	for i, t := range turns {
		for len(speakers) <= t.Speaker {
			speakers = append(speakers, SpeakerLabel(len(speakers)))
		}
		segments[i] = map[string]any{"start": t.Start, "end": t.End, "speaker": SpeakerLabel(t.Speaker)}
	}
	w.Header().Set("X-Oaudio-Model", m.Ref())
	writeJSON(w, http.StatusOK, map[string]any{
		"duration": float64(len(samples)) / float64(m.Audio.SampleRate),
		"speakers": speakers,
		"segments": segments,
	})
}

// DiarizedSegment is a piece of transcript spoken by one speaker.
type DiarizedSegment struct {
	Start, End float64
	Speaker    int
	Text       string
}

// speakerAt returns who is speaking at time t: the turn containing t, or
// failing that the nearest turn, so that words in the small gaps a diarizer
// leaves are still attributed. It returns -1 when there are no turns.
func speakerAt(turns []runner.Turn, t float64) int {
	best, bestDist := -1, 0.0
	for _, turn := range turns {
		if t >= turn.Start && t <= turn.End {
			return turn.Speaker
		}
		d := turn.Start - t
		if t > turn.End {
			d = t - turn.End
		}
		if best < 0 || d < bestDist {
			best, bestDist = turn.Speaker, d
		}
	}
	return best
}

// AssignSpeakers combines a transcript with speaker turns. When the
// transcript has word times, each word goes to whoever is speaking when it
// starts and consecutive words by one speaker form a segment, so a
// transcript segment that spans a change of speaker is split at the change.
// Without word times each transcript segment goes to the speaker who talks
// for most of it.
func AssignSpeakers(segments []runner.Segment, turns []runner.Turn) []DiarizedSegment {
	var out []DiarizedSegment
	add := func(start, end float64, speaker int, text string) {
		if n := len(out); n > 0 && out[n-1].Speaker == speaker {
			out[n-1].End = end
			out[n-1].Text = JoinText(out[n-1].Text, text)
			return
		}
		out = append(out, DiarizedSegment{Start: start, End: end, Speaker: speaker, Text: strings.TrimSpace(text)})
	}
	for _, seg := range segments {
		if len(seg.Words) > 0 {
			for _, w := range seg.Words {
				// A word's start is the reliable end of its time range:
				// recognizers tend to stretch the last word before a pause
				// across the pause, which drags its midpoint toward whoever
				// speaks next.
				add(w.Start, w.End, speakerAt(turns, w.Start+0.02), w.Word)
			}
			continue
		}
		overlap := map[int]float64{}
		for _, turn := range turns {
			lo, hi := maxf(seg.Start, turn.Start), seg.End
			if turn.End < hi {
				hi = turn.End
			}
			if hi > lo {
				overlap[turn.Speaker] += hi - lo
			}
		}
		speaker, most := speakerAt(turns, (seg.Start+seg.End)/2), 0.0
		for sp, d := range overlap {
			if d > most || (d == most && sp < speaker) {
				speaker, most = sp, d
			}
		}
		add(seg.Start, seg.End, speaker, seg.Text)
	}
	return out
}

func diarizedJSON(duration float64, text string, segs []DiarizedSegment) map[string]any {
	list := make([]map[string]any, len(segs))
	for i, seg := range segs {
		speaker := "unknown"
		if seg.Speaker >= 0 {
			speaker = SpeakerLabel(seg.Speaker)
		}
		list[i] = map[string]any{
			"type": "transcript.text.segment", "id": fmt.Sprintf("seg_%d", i),
			"start": seg.Start, "end": seg.End, "speaker": speaker, "text": seg.Text,
		}
	}
	return map[string]any{
		"task": "transcribe", "duration": duration, "text": text, "segments": list,
		"usage": map[string]any{"type": "duration", "seconds": duration},
	}
}

// ---- /v1/audio/voices ----

func voiceObject(v *voices.Voice) map[string]any {
	return map[string]any{
		"id": v.ID, "object": "audio.voice", "type": "audio_sample", "name": v.Name, "created_at": v.CreatedAt,
		// Extensions.
		"consent": v.Consent, "transcript": v.Transcript, "language": v.Language, "seconds": v.Seconds,
	}
}

func (s *Server) handleVoiceCreate(w http.ResponseWriter, r *http.Request) {
	data, err := upload(w, r, "audio_sample")
	if err != nil {
		writeError(w, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	const rate = 24000
	samples, err := audio.Decode(r.Context(), data, rate)
	if err != nil {
		writeError(w, errf(http.StatusBadRequest, "%v", err))
		return
	}
	transcript := strings.TrimSpace(r.FormValue("transcript"))
	language := strings.ToLower(strings.TrimSpace(r.FormValue("language")))
	// Cloning models need to know what the reference says. If the caller
	// does not say, a transcription model can find out.
	if transcript == "" && r.FormValue("transcription_model") != "" {
		stt, err := s.ready(r.FormValue("transcription_model"), manifest.TaskSTT)
		if err != nil {
			writeError(w, err)
			return
		}
		lease, err := s.sched.Acquire(r.Context(), stt, nil)
		if err != nil {
			writeError(w, err)
			return
		}
		pcm := audio.FloatToS16(audio.Resample(samples, rate, stt.Audio.SampleRate))
		_, err = lease.Conn.Transcribe(r.Context(), pcm, runner.TranscribeOptions{SampleRate: stt.Audio.SampleRate, Language: language},
			func(seg runner.Segment) error {
				transcript = JoinText(transcript, seg.Text)
				return nil
			})
		lease.Release()
		if err != nil {
			writeError(w, err)
			return
		}
	}
	v, err := s.voices.Create(r.FormValue("name"), r.FormValue("consent"), transcript, language, samples, rate)
	if err != nil {
		writeError(w, errf(http.StatusBadRequest, "%v", err))
		return
	}
	s.log.Info("voice created", "id", v.ID, "name", v.Name, "seconds", fmt.Sprintf("%.1f", v.Seconds))
	writeJSON(w, http.StatusOK, voiceObject(v))
}

func (s *Server) handleVoiceList(w http.ResponseWriter, r *http.Request) {
	list, err := s.voices.List()
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, len(list))
	for i, v := range list {
		data[i] = voiceObject(v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleVoiceDelete(w http.ResponseWriter, r *http.Request) {
	v, err := s.voices.Get(r.PathValue("id"))
	if err == nil && v == nil {
		err = errf(http.StatusNotFound, "voice %q not found", r.PathValue("id"))
	}
	if err == nil {
		err = s.voices.Delete(v)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": v.ID, "object": "audio.voice.deleted", "deleted": true})
}

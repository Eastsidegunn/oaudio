// Package voices stores custom voices: a short reference recording of a
// person, the words spoken in it, and a record of that person's consent.
//
// A voice is not tied to a model. Any model that declares the voice_clone
// capability can speak with it.
//
//	$OAUDIO_HOME/voices/<id>/voice.json
//	$OAUDIO_HOME/voices/<id>/reference.wav   mono, 16-bit
package voices

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/eastsidegunn/oaudio/internal/audio"
)

// Limits on a reference recording. Cloning models are trained on prompts of a
// few seconds; much longer ones slow synthesis down and tend to sound worse.
const (
	MinSeconds = 1.0
	MaxSeconds = 30.0
	storeRate  = 24000
)

// Voice describes a stored voice.
type Voice struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  int64   `json:"created_at"`
	Consent    string  `json:"consent"`              // who agreed to this voice being copied, as stated at creation
	Transcript string  `json:"transcript,omitempty"` // the words spoken in the reference recording
	Language   string  `json:"language,omitempty"`
	Seconds    float64 `json:"seconds"`
	SampleRate int     `json:"sample_rate"`
}

// Store keeps voices under a directory.
type Store struct {
	Dir string
}

// New returns a store rooted at home/voices.
func New(home string) *Store { return &Store{Dir: filepath.Join(home, "voices")} }

var nameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,63}$`)

// ReferencePath is the recording a runner reads.
func (s *Store) ReferencePath(v *Voice) string {
	return filepath.Join(s.Dir, v.ID, "reference.wav")
}

// Create stores a new voice from mono samples at rate.
func (s *Store) Create(name, consent, transcript, language string, samples []float32, rate int) (*Voice, error) {
	name = strings.TrimSpace(name)
	if !nameRE.MatchString(name) {
		return nil, errors.New("name must be 1 to 64 letters, digits, spaces, dots, dashes or underscores")
	}
	if strings.HasPrefix(name, "voice_") {
		return nil, errors.New("name must not start with voice_, which is reserved for ids")
	}
	if strings.TrimSpace(consent) == "" {
		return nil, errors.New("consent is required: state who agreed to this voice being copied")
	}
	seconds := float64(len(samples)) / float64(rate)
	if seconds < MinSeconds || seconds > MaxSeconds {
		return nil, fmt.Errorf("the reference recording is %.1f s long; it must be between %.0f and %.0f s", seconds, MinSeconds, MaxSeconds)
	}
	if existing, _ := s.Get(name); existing != nil {
		return nil, fmt.Errorf("a voice named %q already exists", name)
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	v := &Voice{
		ID: "voice_" + hex.EncodeToString(raw[:]), Name: name, CreatedAt: time.Now().Unix(),
		Consent: strings.TrimSpace(consent), Transcript: strings.TrimSpace(transcript), Language: language,
		Seconds: seconds, SampleRate: storeRate,
	}
	dir := filepath.Join(s.Dir, v.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pcm := audio.FloatToS16(audio.Resample(samples, rate, storeRate))
	if err := os.WriteFile(s.ReferencePath(v), audio.EncodeWAV(pcm, storeRate), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := s.save(v); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return v, nil
}

func (s *Store) save(v *Voice) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir, v.ID, "voice.json"), append(data, '\n'), 0o600)
}

// SetTranscript records the words of the reference recording.
func (s *Store) SetTranscript(v *Voice, transcript string) error {
	v.Transcript = strings.TrimSpace(transcript)
	return s.save(v)
}

// List returns every voice, oldest first.
func (s *Store) List() ([]*Voice, error) {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "voice_*", "voice.json"))
	if err != nil {
		return nil, err
	}
	var out []*Voice
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var v Voice
		if json.Unmarshal(data, &v) == nil && v.ID == filepath.Base(filepath.Dir(p)) {
			out = append(out, &v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Get finds a voice by id or, case-insensitively, by name. It returns nil
// when there is none.
func (s *Store) Get(idOrName string) (*Voice, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, v := range list {
		if v.ID == idOrName {
			return v, nil
		}
	}
	for _, v := range list {
		if strings.EqualFold(v.Name, idOrName) {
			return v, nil
		}
	}
	return nil, nil
}

// Delete removes a voice and its recording.
func (s *Store) Delete(v *Voice) error {
	if !strings.HasPrefix(v.ID, "voice_") || strings.ContainsAny(v.ID, `/\.`) {
		return errors.New("invalid voice id")
	}
	return os.RemoveAll(filepath.Join(s.Dir, v.ID))
}

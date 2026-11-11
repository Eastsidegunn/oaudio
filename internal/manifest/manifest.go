// Package manifest defines the Oaudio model manifest: a small, human-readable
// JSON document that names a speech model, says which engine runs it, and
// points at the unmodified upstream files with their published hashes.
//
// A manifest never contains weights. Oaudio does not repackage models.
package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Schema is the manifest schema identifier written by this version.
const Schema = "oaudio.manifest.v1"

// Tasks a model can perform.
const (
	TaskSTT     = "stt"
	TaskTTS     = "tts"
	TaskVAD     = "vad"     // find the stretches of audio that contain speech
	TaskDiarize = "diarize" // find who spoke when
)

// Tasks lists every task in display order.
var Tasks = []string{TaskSTT, TaskTTS, TaskVAD, TaskDiarize}

// Capabilities a model may declare. The server rejects requests that need a
// capability the model does not declare instead of silently ignoring them.
const (
	CapSegmentTimestamps = "segment_timestamps"
	CapWordTimestamps    = "word_timestamps"
	CapTranslate         = "translate"
	CapLanguageDetect    = "language_detect"
	CapPrompt            = "prompt"
	CapSpeed             = "speed"
	CapVoiceClone        = "voice_clone" // speaks in a voice taken from a reference recording
)

// Source types.
const (
	SourceHuggingFace = "huggingface"
	SourceURL         = "url"    // files or archives at fixed URLs, each pinned by SHA-256
	SourceSystem      = "system" // provided by the operating system; nothing to download
)

// Manifest describes one runnable model at one tag.
type Manifest struct {
	Schema      string   `json:"schema"`
	Name        string   `json:"name"`
	Tag         string   `json:"tag"`
	Aliases     []string `json:"aliases,omitempty"` // extra tags that resolve here, e.g. "latest"
	Task        string   `json:"task"`
	Engine      string   `json:"engine"`
	Family      string   `json:"family,omitempty"`
	Description string   `json:"description,omitempty"`

	// Upstream credits the people who trained the model. Shown by `oaudio show`.
	Upstream Upstream `json:"upstream"`
	// Source says where the bytes come from and how to verify them.
	Source  Source  `json:"source"`
	License License `json:"license"`

	Languages    []string          `json:"languages,omitempty"` // ISO 639-1; "multi" if many
	Capabilities []string          `json:"capabilities,omitempty"`
	Audio        Audio             `json:"audio"`
	Voices       []Voice           `json:"voices,omitempty"`
	DefaultVoice string            `json:"default_voice,omitempty"`
	Platforms    []string          `json:"platforms,omitempty"` // GOOS/GOARCH globs; empty means any
	EngineConfig map[string]string `json:"engine_config,omitempty"`
}

// Upstream identifies the original model and its authors.
type Upstream struct {
	Model   string `json:"model"`
	Authors string `json:"authors,omitempty"`
	URL     string `json:"url,omitempty"`
}

// Source locates the model files.
type Source struct {
	Type     string `json:"type"`
	Repo     string `json:"repo,omitempty"`
	Revision string `json:"revision,omitempty"` // full commit hash; never a branch name
	Files    []File `json:"files,omitempty"`
}

// File is one upstream file. Exactly one of SHA256 or GitBlobSHA1 is set.
// Hugging Face publishes a SHA-256 for LFS files and a git blob SHA-1 for the
// rest; both are verified against bytes on disk after download.
type File struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	GitBlobSHA1 string `json:"git_blob_sha1,omitempty"`

	// Repo and Revision override the source's for this file, for models
	// assembled from more than one Hugging Face repository.
	Repo     string `json:"repo,omitempty"`
	Revision string `json:"revision,omitempty"`

	// URL fetches the file from a fixed address instead of a repository.
	// It requires SHA256.
	URL string `json:"url,omitempty"`
	// Extract unpacks a downloaded archive ("tar.bz2" or "tar.gz") into the
	// model directory and discards the archive. Path then names the archive
	// only. StripComponents drops leading directories of each entry.
	Extract         string `json:"extract,omitempty"`
	StripComponents int    `json:"strip_components,omitempty"`
	// Exclude skips archive entries whose path (after stripping) starts with
	// one of these prefixes, for parts of an archive the engine never reads.
	Exclude []string `json:"exclude,omitempty"`
}

// License carries what a developer must know before shipping the model.
type License struct {
	SPDX          string             `json:"spdx"`
	URL           string             `json:"url,omitempty"`
	CommercialUse bool               `json:"commercial_use"`
	Gated         bool               `json:"gated,omitempty"`
	Notes         string             `json:"notes,omitempty"`
	Components    []LicenseComponent `json:"components,omitempty"`
}

// LicenseComponent is a bundled file set under a different license.
type LicenseComponent struct {
	Name string `json:"name"`
	SPDX string `json:"spdx"`
	Note string `json:"note,omitempty"`
}

// Audio describes the PCM the runner consumes (stt) or produces (tts).
type Audio struct {
	SampleRate int `json:"sample_rate"`
}

// Voice is a named speaker of a tts model.
type Voice struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Language string `json:"language,omitempty"`
	Gender   string `json:"gender,omitempty"`
}

// Ref is the canonical "name:tag" reference.
func (m *Manifest) Ref() string { return m.Name + ":" + m.Tag }

// Has reports whether the model declares capability c.
func (m *Manifest) Has(c string) bool {
	for _, x := range m.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// TotalSize is the sum of all file sizes in bytes.
func (m *Manifest) TotalSize() int64 {
	var n int64
	for _, f := range m.Source.Files {
		n += f.Size
	}
	return n
}

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	tagRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	hexRE  = regexp.MustCompile(`^[0-9a-f]+$`)
)

// Validate checks a manifest for structural errors.
func (m *Manifest) Validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if m.Schema != Schema {
		add("schema must be %q, got %q", Schema, m.Schema)
	}
	if !nameRE.MatchString(m.Name) {
		add("invalid name %q", m.Name)
	}
	if !tagRE.MatchString(m.Tag) {
		add("invalid tag %q", m.Tag)
	}
	for _, a := range m.Aliases {
		if !tagRE.MatchString(a) {
			add("invalid alias %q", a)
		}
	}
	validTask := false
	for _, t := range Tasks {
		validTask = validTask || t == m.Task
	}
	if !validTask {
		add("task must be one of %s, got %q", strings.Join(Tasks, ", "), m.Task)
	}
	if m.Engine == "" {
		add("engine is required")
	}
	if m.Upstream.Model == "" {
		add("upstream.model is required: credit the original model")
	}
	if m.License.SPDX == "" {
		add("license.spdx is required")
	}
	if m.Audio.SampleRate <= 0 {
		add("audio.sample_rate is required")
	}
	switch m.Source.Type {
	case SourceSystem:
		if len(m.Source.Files) != 0 {
			add("system sources have no files")
		}
	case SourceHuggingFace, SourceURL:
		commit := func(r string) bool { return len(r) == 40 && hexRE.MatchString(r) }
		if m.Source.Type == SourceHuggingFace {
			if m.Source.Repo == "" {
				add("source.repo is required")
			}
			if !commit(m.Source.Revision) {
				add("source.revision must be a full 40-character commit hash, got %q", m.Source.Revision)
			}
		}
		if len(m.Source.Files) == 0 {
			add("source.files is empty")
		}
		seen := map[string]bool{}
		for _, f := range m.Source.Files {
			clean := strings.TrimPrefix(f.Path, "./")
			if f.Path == "" || strings.HasPrefix(f.Path, "/") || strings.Contains("/"+clean+"/", "/../") || strings.Contains(f.Path, "\\") {
				add("unsafe file path %q", f.Path)
			}
			if seen[f.Path] {
				add("duplicate file path %q", f.Path)
			}
			seen[f.Path] = true
			if f.Size < 0 {
				add("%s: negative size", f.Path)
			}
			hasSHA := len(f.SHA256) == 64 && hexRE.MatchString(f.SHA256)
			hasGit := len(f.GitBlobSHA1) == 40 && hexRE.MatchString(f.GitBlobSHA1)
			if hasSHA == hasGit {
				add("%s: exactly one of sha256 or git_blob_sha1 is required", f.Path)
			}
			if f.Revision != "" && !commit(f.Revision) {
				add("%s: revision must be a full 40-character commit hash", f.Path)
			}
			if (f.Repo == "") != (f.Revision == "") {
				add("%s: repo and revision must be set together", f.Path)
			}
			if f.URL != "" {
				if !strings.HasPrefix(f.URL, "https://") {
					add("%s: url must use https", f.Path)
				}
				if !hasSHA {
					add("%s: a url needs a sha256", f.Path)
				}
			} else if m.Source.Type == SourceURL {
				add("%s: url is required for url sources", f.Path)
			}
			switch f.Extract {
			case "":
			case "tar.bz2", "tar.gz":
				if !hasSHA {
					add("%s: an archive needs a sha256", f.Path)
				}
			default:
				add("%s: unknown archive type %q", f.Path, f.Extract)
			}
			if f.StripComponents < 0 {
				add("%s: negative strip_components", f.Path)
			}
		}
	default:
		add("unknown source.type %q", m.Source.Type)
	}
	if m.Task == TaskTTS && m.DefaultVoice != "" && len(m.Voices) > 0 {
		ok := false
		for _, v := range m.Voices {
			if v.ID == m.DefaultVoice {
				ok = true
			}
		}
		if !ok {
			add("default_voice %q is not in voices", m.DefaultVoice)
		}
	}
	if len(errs) > 0 {
		return errors.New(m.Name + ":" + m.Tag + ": " + strings.Join(errs, "; "))
	}
	return nil
}

// Parse decodes and validates a manifest. Unknown fields are rejected so that
// typos do not silently change behaviour.
func Parse(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Load reads a manifest file.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// SplitRef splits "name[:tag]" into its parts. A missing tag is "latest".
func SplitRef(ref string) (name, tag string, err error) {
	ref = strings.TrimSpace(ref)
	name, tag, ok := strings.Cut(ref, ":")
	if !ok || tag == "" {
		tag = "latest"
	}
	if !nameRE.MatchString(name) {
		return "", "", fmt.Errorf("invalid model name %q", name)
	}
	if !tagRE.MatchString(tag) {
		return "", "", fmt.Errorf("invalid model tag %q", tag)
	}
	return name, tag, nil
}

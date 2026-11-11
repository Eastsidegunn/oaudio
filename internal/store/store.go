// Package store manages models on disk.
//
// Layout, deliberately boring:
//
//	$OAUDIO_HOME/models/<name>/<tag>/manifest.json
//	$OAUDIO_HOME/models/<name>/<tag>/<upstream file paths, unmodified>
//
// Files keep their upstream names and bytes, so any other tool can use them
// directly (for example `whisper-cli -m $(oaudio show whisper --path)/ggml-base.en.bin`).
package store

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/eastsidegunn/oaudio/internal/catalog"
	"github.com/eastsidegunn/oaudio/internal/manifest"
)

// Store is rooted at an Oaudio home directory.
type Store struct {
	Home string
}

// DefaultHome returns $OAUDIO_HOME or ~/.oaudio.
func DefaultHome() string {
	if h := os.Getenv("OAUDIO_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".oaudio"
	}
	return filepath.Join(home, ".oaudio")
}

// New returns a store rooted at home.
func New(home string) *Store { return &Store{Home: home} }

// ManifestsDir is where user-supplied manifests live.
func (s *Store) ManifestsDir() string { return filepath.Join(s.Home, "manifests") }

// ModelDir is the directory holding the files of m.
func (s *Store) ModelDir(m *manifest.Manifest) string {
	return filepath.Join(s.Home, "models", m.Name, m.Tag)
}

func (s *Store) manifestPath(m *manifest.Manifest) string {
	return filepath.Join(s.ModelDir(m), "manifest.json")
}

// IsInstalled reports whether m is ready to run. System models need no files.
func (s *Store) IsInstalled(m *manifest.Manifest) bool {
	if m.Source.Type == manifest.SourceSystem {
		return catalog.SupportsPlatform(m)
	}
	_, err := os.Stat(s.manifestPath(m))
	return err == nil
}

// MarkInstalled records the manifest next to the files. Call only after every
// file has been verified.
func (s *Store) MarkInstalled(m *manifest.Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.ModelDir(m), 0o755); err != nil {
		return err
	}
	tmp := s.manifestPath(m) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.manifestPath(m))
}

// Installed lists downloaded models, read from the manifests on disk (not the
// catalog), so a model stays usable even if a later release drops its entry.
func (s *Store) Installed() ([]*manifest.Manifest, error) {
	paths, err := filepath.Glob(filepath.Join(s.Home, "models", "*", "*", "manifest.json"))
	if err != nil {
		return nil, err
	}
	var out []*manifest.Manifest
	for _, p := range paths {
		m, err := manifest.Load(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "oaudio: skipping damaged install: %v\n", err)
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref() < out[j].Ref() })
	return out, nil
}

// Remove deletes the files of m.
func (s *Store) Remove(m *manifest.Manifest) error {
	if m.Source.Type == manifest.SourceSystem {
		return errors.New("system models are provided by the operating system and cannot be removed")
	}
	dir := s.ModelDir(m)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%s is not installed", m.Ref())
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// Drop the now-empty <name> directory; ignore failure if other tags remain.
	_ = os.Remove(filepath.Dir(dir))
	return nil
}

// Verify re-hashes every file of an installed model. Files that came out of
// an archive are checked against the hashes recorded when it was unpacked.
func (s *Store) Verify(m *manifest.Manifest) error {
	for _, f := range m.Source.Files {
		if f.Extract != "" {
			if err := VerifyArchive(s.ModelDir(m), f); err != nil {
				return err
			}
			continue
		}
		if err := VerifyFile(filepath.Join(s.ModelDir(m), filepath.FromSlash(f.Path)), f); err != nil {
			return err
		}
	}
	return nil
}

// FileCount is the number of files a model has on disk.
func (s *Store) FileCount(m *manifest.Manifest) int {
	n := 0
	for _, f := range m.Source.Files {
		if f.Extract == "" {
			n++
		} else if rec, err := readArchiveRecord(s.ModelDir(m), f); err == nil {
			n += len(rec.Files)
		}
	}
	return n
}

// ArchiveRecord remembers what an archive contained. The archive itself is
// verified against the manifest and then deleted; this record lets a later
// `oaudio verify` detect changes to the unpacked files.
type ArchiveRecord struct {
	Archive string            `json:"archive"`
	SHA256  string            `json:"sha256"`
	Files   map[string]string `json:"files"` // relative path -> sha256
}

func archiveRecordPath(dir string, f manifest.File) string {
	return filepath.Join(dir, ".oaudio-archive-"+f.SHA256[:16]+".json")
}

func readArchiveRecord(dir string, f manifest.File) (*ArchiveRecord, error) {
	data, err := os.ReadFile(archiveRecordPath(dir, f))
	if err != nil {
		return nil, err
	}
	var rec ArchiveRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.SHA256 != f.SHA256 {
		return nil, fmt.Errorf("%s: archive record is for a different archive", f.Path)
	}
	return &rec, nil
}

// WriteArchiveRecord stores the record for an unpacked archive.
func WriteArchiveRecord(dir string, f manifest.File, files map[string]string) error {
	data, err := json.MarshalIndent(ArchiveRecord{Archive: f.Path, SHA256: f.SHA256, Files: files}, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(archiveRecordPath(dir, f), data, 0o644)
}

// VerifyArchive checks the unpacked files of an archive.
func VerifyArchive(dir string, f manifest.File) error {
	rec, err := readArchiveRecord(dir, f)
	if err != nil {
		return fmt.Errorf("%s: not unpacked: %w", f.Path, err)
	}
	for rel, want := range rec.Files {
		got, err := SHA256File(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s: changed since it was unpacked from %s", rel, f.Path)
		}
	}
	return nil
}

// SHA256File hashes a file.
func SHA256File(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyFile checks size and hash of one file against the manifest entry.
func VerifyFile(path string, f manifest.File) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	if st.Size() != f.Size {
		return fmt.Errorf("%s: size %d, upstream says %d", f.Path, st.Size(), f.Size)
	}
	var h hash.Hash
	want := f.SHA256
	if f.SHA256 != "" {
		h = sha256.New()
	} else {
		// A git blob id is sha1("blob <size>\0" + content).
		h = sha1.New()
		fmt.Fprintf(h, "blob %d\x00", st.Size())
		want = f.GitBlobSHA1
	}
	if _, err := io.Copy(h, fh); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: hash mismatch: got %s, upstream publishes %s", f.Path, got, want)
	}
	return nil
}

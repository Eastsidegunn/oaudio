package pull

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/internal/store"
)

const rev = "0123456789abcdef0123456789abcdef01234567"

func sha256hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func gitBlob(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func testManifest(files map[string][]byte) *manifest.Manifest {
	m := &manifest.Manifest{Schema: manifest.Schema, Name: "m", Tag: "t", Task: "stt", Engine: "e",
		Upstream: manifest.Upstream{Model: "org/m"}, License: manifest.License{SPDX: "MIT"},
		Audio:  manifest.Audio{SampleRate: 16000},
		Source: manifest.Source{Type: manifest.SourceHuggingFace, Repo: "org/m", Revision: rev}}
	for p, b := range files {
		f := manifest.File{Path: p, Size: int64(len(b))}
		if strings.HasSuffix(p, ".bin") {
			f.SHA256 = sha256hex(b) // large files carry a SHA-256
		} else {
			f.GitBlobSHA1 = gitBlob(b) // small files carry a git blob id
		}
		m.Source.Files = append(m.Source.Files, f)
	}
	return m
}

type hubLog struct {
	mu     sync.Mutex
	ranges []string
}

// hub imitates the Hugging Face file endpoint, including Range support.
func hub(t *testing.T, served map[string][]byte) *hubLog {
	log := &hubLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/org/m/resolve/" + rev + "/"
		b, ok := served[strings.TrimPrefix(r.URL.Path, prefix)]
		if !ok || !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		log.mu.Lock()
		log.ranges = append(log.ranges, r.Header.Get("Range"))
		log.mu.Unlock()
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HF_ENDPOINT", srv.URL)
	return log
}

func TestPullVerifiesAndInstalls(t *testing.T) {
	files := map[string][]byte{
		"model.bin":      bytes.Repeat([]byte("weights"), 5000),
		"sub/tokens.txt": []byte("a\nb\nc\n"),
	}
	hub(t, files)
	st := store.New(t.TempDir())
	m := testManifest(files)
	if err := Model(context.Background(), st, m, nil); err != nil {
		t.Fatal(err)
	}
	if !st.IsInstalled(m) {
		t.Fatal("not marked installed")
	}
	// Files keep their upstream names and bytes.
	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(st.ModelDir(m), filepath.FromSlash(p)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: differs from upstream (err=%v)", p, err)
		}
	}
	if err := st.Verify(m); err != nil {
		t.Errorf("verify: %v", err)
	}
	// Tampering after install is caught by verify.
	os.WriteFile(filepath.Join(st.ModelDir(m), "sub", "tokens.txt"), []byte("a\nb\nX\n"), 0o644)
	if err := st.Verify(m); err == nil {
		t.Error("verify accepted a modified file")
	}
}

// If the host serves bytes that do not match the manifest hash, nothing is
// installed and the bad file is not left behind.
func TestPullRejectsWrongContent(t *testing.T) {
	good := map[string][]byte{"model.bin": bytes.Repeat([]byte("a"), 4096)}
	evil := map[string][]byte{"model.bin": bytes.Repeat([]byte("b"), 4096)} // same size, different bytes
	hub(t, evil)
	st := store.New(t.TempDir())
	m := testManifest(good)
	err := Model(context.Background(), st, m, nil)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("got %v, want a hash mismatch", err)
	}
	if st.IsInstalled(m) {
		t.Error("model marked installed despite a bad hash")
	}
	left, _ := filepath.Glob(filepath.Join(st.ModelDir(m), "*"))
	if len(left) != 0 {
		t.Errorf("bad download left on disk: %v", left)
	}
}

// A partial download is resumed with a Range request, not restarted.
func TestPullResumes(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789"), 10000)
	files := map[string][]byte{"model.bin": content}
	log := hub(t, files)
	st := store.New(t.TempDir())
	m := testManifest(files)
	os.MkdirAll(st.ModelDir(m), 0o755)
	os.WriteFile(filepath.Join(st.ModelDir(m), "model.bin.partial"), content[:30000], 0o644)
	if err := Model(context.Background(), st, m, nil); err != nil {
		t.Fatal(err)
	}
	if len(log.ranges) != 1 || log.ranges[0] != "bytes=30000-" {
		t.Errorf("requests %q, want one resume from byte 30000", log.ranges)
	}
	if err := st.Verify(m); err != nil {
		t.Error(err)
	}
	// A second pull downloads nothing.
	if err := Model(context.Background(), st, m, nil); err != nil {
		t.Fatal(err)
	}
	if len(log.ranges) != 1 {
		t.Errorf("second pull made %d extra requests", len(log.ranges)-1)
	}
}

func TestPullMissingFile(t *testing.T) {
	hub(t, map[string][]byte{})
	st := store.New(t.TempDir())
	m := testManifest(map[string][]byte{"model.bin": []byte("x")})
	if err := Model(context.Background(), st, m, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("got %v, want a 404 error", err)
	}
}

func tarGz(t *testing.T, entries map[string]string, links map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	for name, target := range links {
		tw.WriteHeader(&tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink})
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func archiveManifest(archive []byte) *manifest.Manifest {
	m := testManifest(map[string][]byte{})
	m.Source.Files = []manifest.File{{Path: "model.bin", Size: int64(len(archive)), SHA256: sha256hex(archive),
		Extract: "tar.gz", StripComponents: 1}}
	return m
}

// An archive is verified as a whole, unpacked, and its files remain checkable.
func TestPullArchive(t *testing.T) {
	archive := tarGz(t, map[string]string{"top/model.onnx": "weights", "top/sub/tokens.txt": "a b c"},
		map[string]string{"top/link": "/etc/passwd"})
	hub(t, map[string][]byte{"model.bin": archive})
	st := store.New(t.TempDir())
	m := archiveManifest(archive)
	if err := Model(context.Background(), st, m, nil); err != nil {
		t.Fatal(err)
	}
	dir := st.ModelDir(m)
	if got, _ := os.ReadFile(filepath.Join(dir, "sub", "tokens.txt")); string(got) != "a b c" {
		t.Errorf("unpacked file content %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Error("a symlink from the archive was created")
	}
	if _, err := os.Stat(filepath.Join(dir, "model.bin")); err == nil {
		t.Error("the archive was left behind after unpacking")
	}
	if err := st.Verify(m); err != nil || st.FileCount(m) != 2 {
		t.Errorf("verify: %v, files: %d", err, st.FileCount(m))
	}
	os.WriteFile(filepath.Join(dir, "model.onnx"), []byte("tampered"), 0o644)
	if err := st.Verify(m); err == nil {
		t.Error("verify accepted a modified unpacked file")
	}
}

// An archive entry may not write outside the model directory.
func TestPullArchiveTraversal(t *testing.T) {
	archive := tarGz(t, map[string]string{"top/../../../escaped.txt": "x"}, nil)
	hub(t, map[string][]byte{"model.bin": archive})
	home := t.TempDir()
	st := store.New(home)
	m := archiveManifest(archive)
	err := Model(context.Background(), st, m, nil)
	if _, statErr := os.Stat(filepath.Join(home, "escaped.txt")); statErr == nil {
		t.Fatal("an archive entry escaped the model directory")
	}
	found, _ := filepath.Glob(filepath.Join(home, "**", "escaped.txt"))
	if err == nil && len(found) > 0 {
		t.Errorf("traversal entry was written: %v", found)
	}
	if st.IsInstalled(m) && err != nil {
		t.Error("model installed despite an extraction error")
	}
}

package catalog

import (
	"testing"

	"github.com/eastsidegunn/oaudio/internal/manifest"
)

// Every manifest shipped in the binary must be valid, credit its upstream,
// state a license, and pin its files to a commit and a hash.
func TestEmbeddedCatalog(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.All()) == 0 {
		t.Fatal("empty catalog")
	}
	latest := map[string]int{}
	for _, m := range c.All() {
		if m.Upstream.Model == "" || m.License.SPDX == "" {
			t.Errorf("%s: missing upstream credit or license", m.Ref())
		}
		for _, a := range m.Aliases {
			if a == "latest" {
				latest[m.Name]++
			}
		}
		if m.Source.Type == manifest.SourceHuggingFace {
			for _, f := range m.Source.Files {
				if f.SHA256 == "" && f.GitBlobSHA1 == "" {
					t.Errorf("%s: %s has no hash", m.Ref(), f.Path)
				}
			}
		}
	}
	for _, m := range c.All() {
		if latest[m.Name] != 1 {
			t.Errorf("%s: %d tags claim to be latest, want exactly 1", m.Name, latest[m.Name])
		}
	}
}

func TestResolve(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.Resolve("whisper")
	if err != nil || m.Tag != "large-v3-turbo-q5_0" {
		t.Fatalf("whisper -> %v, %v", m, err)
	}
	if m, err := c.Resolve("whisper:base.en-q5_1"); err != nil || m.Tag != "base.en-q5_1" {
		t.Fatalf("exact tag: %v, %v", m, err)
	}
	if _, err := c.Resolve("whisper:nope"); err == nil {
		t.Error("unknown tag resolved")
	}
	if _, err := c.Resolve("nope"); err == nil {
		t.Error("unknown model resolved")
	}
	if _, err := c.Resolve("../etc"); err == nil {
		t.Error("path-like name accepted")
	}
}

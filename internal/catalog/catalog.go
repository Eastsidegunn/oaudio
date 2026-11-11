// Package catalog resolves "name:tag" to a manifest.
//
// Manifests come from two places: the set embedded in the binary, and any
// *.json file the user drops in $OAUDIO_HOME/manifests. User manifests win,
// so nobody has to wait for a release to run a new model.
package catalog

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/eastsidegunn/oaudio/internal/manifest"
)

//go:embed models/*.json
var embedded embed.FS

// Catalog is an immutable set of manifests.
type Catalog struct {
	all []*manifest.Manifest
}

// Load builds a catalog from the embedded manifests plus userDir (may be "").
func Load(userDir string) (*Catalog, error) {
	byRef := map[string]*manifest.Manifest{}
	entries, err := fs.ReadDir(embedded, "models")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		data, err := embedded.ReadFile(path.Join("models", e.Name()))
		if err != nil {
			return nil, err
		}
		m, err := manifest.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("embedded %s: %w", e.Name(), err)
		}
		if _, dup := byRef[m.Ref()]; dup {
			return nil, fmt.Errorf("embedded %s: duplicate %s", e.Name(), m.Ref())
		}
		byRef[m.Ref()] = m
	}
	if userDir != "" {
		files, _ := filepath.Glob(filepath.Join(userDir, "*.json"))
		for _, f := range files {
			m, err := manifest.Load(f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "oaudio: ignoring user manifest: %v\n", err)
				continue
			}
			byRef[m.Ref()] = m
		}
	}
	c := &Catalog{}
	for _, m := range byRef {
		c.all = append(c.all, m)
	}
	sort.Slice(c.all, func(i, j int) bool { return c.all[i].Ref() < c.all[j].Ref() })
	return c, nil
}

// All returns every manifest, sorted by reference.
func (c *Catalog) All() []*manifest.Manifest { return c.all }

// Resolve finds the manifest for "name[:tag]". Tags match exactly or through
// an alias such as "latest".
func (c *Catalog) Resolve(ref string) (*manifest.Manifest, error) {
	name, tag, err := manifest.SplitRef(ref)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, m := range c.all {
		if m.Name != name {
			continue
		}
		if m.Tag == tag {
			return m, nil
		}
		tags = append(tags, m.Tag)
	}
	for _, m := range c.all {
		if m.Name != name {
			continue
		}
		for _, a := range m.Aliases {
			if a == tag {
				return m, nil
			}
		}
	}
	if len(tags) > 0 {
		return nil, fmt.Errorf("model %q has no tag %q (available: %s)", name, tag, strings.Join(tags, ", "))
	}
	return nil, fmt.Errorf("model %q not found; run `oaudio catalog` to see available models", name)
}

// SupportsPlatform reports whether m can run on this machine.
func SupportsPlatform(m *manifest.Manifest) bool {
	if len(m.Platforms) == 0 {
		return true
	}
	here := runtime.GOOS + "/" + runtime.GOARCH
	for _, p := range m.Platforms {
		if ok, _ := path.Match(p, here); ok {
			return true
		}
	}
	return false
}

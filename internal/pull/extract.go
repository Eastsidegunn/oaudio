package pull

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/eastsidegunn/oaudio/internal/manifest"
)

// extract unpacks a verified archive into dir and returns the SHA-256 of
// every file it wrote. Only regular files and directories are created;
// entries that would land outside dir are an error, and links are skipped,
// so an archive cannot place or point at anything beyond the model directory.
func extract(archive, dir string, f manifest.File) (map[string]string, error) {
	fh, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var r io.Reader
	switch f.Extract {
	case "tar.bz2":
		r = bzip2.NewReader(fh)
	case "tar.gz":
		gz, err := gzip.NewReader(fh)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	default:
		return nil, fmt.Errorf("unknown archive type %q", f.Extract)
	}
	files := map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("corrupt archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		parts := strings.Split(name, "/")
		if len(parts) <= f.StripComponents {
			continue
		}
		rel := path.Join(parts[f.StripComponents:]...)
		if rel == "." || rel == "" {
			continue
		}
		if path.IsAbs(name) || strings.HasPrefix(rel, "../") || rel == ".." || strings.Contains(rel, "\\") {
			return nil, fmt.Errorf("archive entry %q escapes the model directory", hdr.Name)
		}
		skip := false
		for _, prefix := range f.Exclude {
			skip = skip || strings.HasPrefix(rel, prefix) || rel+"/" == prefix
		}
		if skip {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return nil, err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return nil, err
			}
			h := sha256.New()
			if _, err := io.Copy(io.MultiWriter(out, h), tr); err != nil {
				out.Close()
				return nil, err
			}
			if err := out.Close(); err != nil {
				return nil, err
			}
			files[rel] = hex.EncodeToString(h.Sum(nil))
		default:
			// Symlinks, hard links and devices are never created.
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("archive contained no files")
	}
	return files, nil
}

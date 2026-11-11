// Package pull downloads model files from their upstream host and verifies
// every byte against the hash the upstream publishes.
package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/internal/store"
)

// Progress reports download state. Completed and Total are bytes across all files.
type Progress struct {
	Status    string `json:"status"`
	File      string `json:"file,omitempty"`
	Completed int64  `json:"completed"`
	Total     int64  `json:"total"`
}

// Endpoint returns the Hugging Face base URL (override with HF_ENDPOINT).
func Endpoint() string {
	if e := os.Getenv("HF_ENDPOINT"); e != "" {
		return strings.TrimRight(e, "/")
	}
	return "https://huggingface.co"
}

func fileURL(src manifest.Source, f manifest.File) string {
	if f.URL != "" {
		return f.URL
	}
	repo, rev := src.Repo, src.Revision
	if f.Repo != "" {
		repo, rev = f.Repo, f.Revision
	}
	segs := strings.Split(f.Path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/resolve/%s/%s", Endpoint(), repo, rev, strings.Join(segs, "/"))
}

var client = &http.Client{Timeout: 0, Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	ResponseHeaderTimeout: 60 * time.Second,
	MaxIdleConnsPerHost:   4,
}}

func newRequest(ctx context.Context, u string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "oaudio")
	if t := os.Getenv("HF_TOKEN"); t != "" && strings.HasPrefix(u, Endpoint()) {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return req, nil
}

// Model downloads every file of m into the store, verifies them, and marks
// the model installed. Files already present and valid are not fetched again.
func Model(ctx context.Context, st *store.Store, m *manifest.Manifest, report func(Progress)) error {
	if report == nil {
		report = func(Progress) {}
	}
	if m.Source.Type == manifest.SourceSystem {
		report(Progress{Status: "provided by the operating system; nothing to download"})
		return nil
	}
	dir := st.ModelDir(m)
	total := m.TotalSize()
	var done int64
	for _, f := range m.Source.Files {
		dst := filepath.Join(dir, filepath.FromSlash(f.Path))
		present := store.VerifyFile(dst, f) == nil
		if f.Extract != "" {
			present = store.VerifyArchive(dir, f) == nil
		}
		if present {
			done += f.Size
			report(Progress{Status: "already present", File: f.Path, Completed: done, Total: total})
			continue
		}
		base := done
		err := fetch(ctx, fileURL(m.Source, f), dst, f, func(n int64) {
			report(Progress{Status: "downloading", File: f.Path, Completed: base + n, Total: total})
		})
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
		done += f.Size
		if f.Extract != "" {
			// The archive has just been verified against the manifest hash.
			report(Progress{Status: "unpacking", File: f.Path, Completed: done, Total: total})
			files, err := extract(dst, dir, f)
			if err != nil {
				return fmt.Errorf("%s: %w", f.Path, err)
			}
			if err := store.WriteArchiveRecord(dir, f, files); err != nil {
				return err
			}
			os.Remove(dst)
		}
	}
	report(Progress{Status: "verified", Completed: total, Total: total})
	if err := st.MarkInstalled(m); err != nil {
		return err
	}
	report(Progress{Status: "success", Completed: total, Total: total})
	return nil
}

// fetch downloads u to dst through dst+".partial", resuming a previous
// partial download, and renames into place only after the hash matches.
func fetch(ctx context.Context, u, dst string, f manifest.File, onBytes func(int64)) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	part := dst + ".partial"
	for attempt := 0; ; attempt++ {
		err := fetchOnce(ctx, u, part, f, onBytes)
		if err == nil {
			break
		}
		if ctx.Err() != nil || attempt >= 4 || errors.As(err, new(permanentError)) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if err := store.VerifyFile(part, f); err != nil {
		os.Remove(part) // corrupt or wrong content: never keep it
		return err
	}
	return os.Rename(part, dst)
}

type permanentError struct{ error }

func fetchOnce(ctx context.Context, u, part string, f manifest.File, onBytes func(int64)) error {
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
		if have > f.Size {
			os.Remove(part)
			have = 0
		}
	}
	if have == f.Size {
		onBytes(have)
		return nil
	}
	req, err := newRequest(ctx, u)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		flags |= os.O_TRUNC
		have = 0
	case http.StatusUnauthorized, http.StatusForbidden:
		return permanentError{fmt.Errorf("upstream returned %s: this model may be gated; accept its terms on the model page and set HF_TOKEN", resp.Status)}
	case http.StatusNotFound:
		return permanentError{fmt.Errorf("upstream returned %s for %s", resp.Status, u)}
	default:
		return fmt.Errorf("upstream returned %s", resp.Status)
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return permanentError{err}
	}
	defer out.Close()
	buf := make([]byte, 1<<20)
	last := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return permanentError{werr}
			}
			have += int64(n)
			if time.Since(last) > 100*time.Millisecond {
				onBytes(have)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	onBytes(have)
	if have != f.Size {
		return fmt.Errorf("short download: %d of %d bytes", have, f.Size)
	}
	return nil
}

// HFFiles lists the files of a Hugging Face repo at a revision, with the
// hashes the Hub publishes. It is used to write manifests, never to trust
// content: the hashes go into a reviewed manifest file.
func HFFiles(ctx context.Context, repo, revision string, include func(path string) bool) (commit string, files []manifest.File, err error) {
	var info struct {
		SHA string `json:"sha"`
	}
	if err := getJSON(ctx, fmt.Sprintf("%s/api/models/%s/revision/%s", Endpoint(), repo, url.PathEscape(revision)), &info); err != nil {
		return "", nil, err
	}
	if len(info.SHA) != 40 {
		return "", nil, fmt.Errorf("could not resolve %s@%s to a commit", repo, revision)
	}
	next := fmt.Sprintf("%s/api/models/%s/tree/%s?recursive=true&limit=1000", Endpoint(), repo, info.SHA)
	for next != "" {
		var page []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
			OID  string `json:"oid"`
			LFS  *struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"lfs"`
		}
		link, err := getJSONLink(ctx, next, &page)
		if err != nil {
			return "", nil, err
		}
		for _, e := range page {
			if e.Type != "file" || (include != nil && !include(e.Path)) {
				continue
			}
			f := manifest.File{Path: e.Path, Size: e.Size}
			if e.LFS != nil {
				f.SHA256, f.Size = e.LFS.OID, e.LFS.Size
			} else {
				f.GitBlobSHA1 = e.OID
			}
			files = append(files, f)
		}
		next = link
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return info.SHA, files, nil
}

func getJSON(ctx context.Context, u string, v any) error {
	_, err := getJSONLink(ctx, u, v)
	return err
}

func getJSONLink(ctx context.Context, u string, v any) (next string, err error) {
	req, err := newRequest(ctx, u)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", u, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return "", err
	}
	// Link: <url>; rel="next"
	for _, l := range resp.Header.Values("Link") {
		for _, part := range strings.Split(l, ",") {
			if strings.Contains(part, `rel="next"`) {
				if i, j := strings.Index(part, "<"), strings.Index(part, ">"); i >= 0 && j > i {
					next = part[i+1 : j]
				}
			}
		}
	}
	return next, nil
}

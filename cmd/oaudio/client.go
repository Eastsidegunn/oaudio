package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eastsidegunn/oaudio/internal/manifest"
	"github.com/eastsidegunn/oaudio/internal/pull"
	"github.com/eastsidegunn/oaudio/internal/server"
	"github.com/eastsidegunn/oaudio/internal/store"
)

func baseURL() string { return "http://" + server.Addr() }

// parseArgs separates flags from positional arguments so flags may appear
// anywhere on the command line. short maps one-letter flags to long names.
func parseArgs(args []string, boolFlags map[string]bool, short map[string]string) (opts map[string]string, pos []string, err error) {
	opts = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		if long, ok := short[name]; ok {
			name = long
		}
		if boolFlags[name] {
			opts[name] = "true"
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %s needs a value", a)
			}
			i++
			val = args[i]
		}
		opts[name] = val
	}
	return opts, pos, nil
}

// ensureServer makes sure a daemon is reachable, starting one in the
// background if the address is local and nothing answers.
func ensureServer() error {
	if ping() == nil {
		return nil
	}
	host := server.Addr()
	if !strings.HasPrefix(host, "127.0.0.1:") && !strings.HasPrefix(host, "localhost:") {
		return fmt.Errorf("cannot reach the oaudio server at %s", host)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logDir := filepath.Join(store.DefaultHome(), "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	cmd.Process.Release()
	for i := 0; i < 100; i++ {
		time.Sleep(50 * time.Millisecond)
		if ping() == nil {
			return nil
		}
	}
	return fmt.Errorf("the server did not start; see %s", logFile.Name())
}

func ping() error {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(baseURL() + "/api/version")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// apiError extracts the message from an error response.
func apiError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error.Message != "" {
		return errors.New(body.Error.Message)
	}
	return fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
}

func getJSON(path string, v any) error {
	resp, err := http.Get(baseURL() + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type summary struct {
	Name      string           `json:"name"`
	Task      string           `json:"task"`
	Engine    string           `json:"engine"`
	Size      int64            `json:"size"`
	Languages []string         `json:"languages"`
	License   manifest.License `json:"license"`
	Upstream  string           `json:"upstream"`
	Installed bool             `json:"installed"`
	Supported bool             `json:"supported"`
	Aliases   []string         `json:"aliases"`
	EngineErr string           `json:"engine_error"`
}

func humanSize(n int64) string {
	switch {
	case n == 0:
		return "-"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
}

func licenseLabel(l manifest.License) string {
	s := l.SPDX
	if !l.CommercialUse {
		s += " (non-commercial)"
	}
	return s
}

func langLabel(l []string) string {
	if len(l) > 4 {
		return fmt.Sprintf("%s +%d", strings.Join(l[:3], ","), len(l)-3)
	}
	return strings.Join(l, ",")
}

func cmdList(all bool) error {
	if err := ensureServer(); err != nil {
		return err
	}
	path := "/api/models"
	if all {
		path = "/api/catalog"
	}
	var resp struct {
		Models []summary `json:"models"`
	}
	if err := getJSON(path, &resp); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	if all {
		fmt.Fprintln(tw, "NAME\tTASK\tENGINE\tSIZE\tLANGUAGES\tLICENSE\tSTATUS")
	} else {
		fmt.Fprintln(tw, "NAME\tTASK\tENGINE\tSIZE\tLANGUAGES\tLICENSE")
	}
	for _, m := range resp.Models {
		name := m.Name
		for _, a := range m.Aliases {
			if a == "latest" {
				name += " (latest)"
			}
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", name, m.Task, m.Engine, humanSize(m.Size), langLabel(m.Languages), licenseLabel(m.License))
		if all {
			status := "available"
			switch {
			case !m.Supported:
				status = "not for this platform"
			case m.EngineErr != "":
				status = "engine not installed"
				if m.Installed {
					status = "downloaded, engine not installed"
				}
			case m.Installed:
				status = "installed"
			}
			row += "\t" + status
		}
		fmt.Fprintln(tw, row)
	}
	return tw.Flush()
}

func cmdPs() error {
	if err := ensureServer(); err != nil {
		return err
	}
	var resp struct {
		Models []struct {
			Model     string    `json:"model"`
			Task      string    `json:"task"`
			Engine    string    `json:"engine"`
			Pid       int       `json:"pid"`
			InUse     bool      `json:"in_use"`
			Loading   bool      `json:"loading"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"models"`
	}
	if err := getJSON("/api/ps", &resp); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTASK\tENGINE\tPID\tUNTIL")
	for _, m := range resp.Models {
		until := "forever"
		switch {
		case m.Loading:
			until = "loading"
		case m.InUse:
			until = "in use"
		case !m.ExpiresAt.IsZero():
			until = time.Until(m.ExpiresAt).Round(time.Second).String() + " from now"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", m.Model, m.Task, m.Engine, m.Pid, until)
	}
	return tw.Flush()
}

func cmdPull(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: oaudio pull <model>")
	}
	if err := ensureServer(); err != nil {
		return err
	}
	return pullModel(args[0])
}

func pullModel(model string) error {
	body, _ := json.Marshal(map[string]string{"model": model})
	resp, err := http.Post(baseURL()+"/api/pull", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	lastLen := 0
	for sc.Scan() {
		var p struct {
			pull.Progress
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &p) != nil {
			continue
		}
		if p.Error != "" {
			fmt.Fprintln(os.Stderr)
			return errors.New(p.Error)
		}
		line := p.Status
		if p.Status == "downloading" && p.Total > 0 {
			line = fmt.Sprintf("downloading %s  %3.0f%%  %s / %s", p.File, 100*float64(p.Completed)/float64(p.Total), humanSize(p.Completed), humanSize(p.Total))
		} else if p.File != "" {
			line = p.Status + " " + p.File
		}
		pad := ""
		if n := lastLen - len(line); n > 0 {
			pad = strings.Repeat(" ", n)
		}
		fmt.Fprintf(os.Stderr, "\r%s%s", line, pad)
		lastLen = len(line)
	}
	fmt.Fprintln(os.Stderr)
	return sc.Err()
}

type showResponse struct {
	Manifest    manifest.Manifest `json:"manifest"`
	Installed   bool              `json:"installed"`
	Supported   bool              `json:"supported"`
	Path        string            `json:"path"`
	EngineError string            `json:"engine_error"`
}

func show(model string) (*showResponse, error) {
	var resp showResponse
	if err := getJSON("/api/show?model="+url.QueryEscape(model), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func cmdShow(args []string) error {
	opts, pos, err := parseArgs(args, map[string]bool{"path": true, "voices": true, "json": true}, nil)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: oaudio show <model> [--path | --voices | --json]")
	}
	if err := ensureServer(); err != nil {
		return err
	}
	info, err := show(pos[0])
	if err != nil {
		return err
	}
	m := info.Manifest
	switch {
	case opts["path"] != "":
		if info.Path == "" {
			return errors.New("this model is provided by the operating system and has no files")
		}
		fmt.Println(info.Path)
		return nil
	case opts["json"] != "":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(m)
	case opts["voices"] != "":
		var v struct {
			Default string           `json:"default_voice"`
			Voices  []manifest.Voice `json:"voices"`
		}
		if err := getJSON("/api/voices?model="+url.QueryEscape(pos[0]), &v); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "VOICE\tLANGUAGE")
		for _, voice := range v.Voices {
			mark := ""
			if voice.ID == v.Default {
				mark = " (default)"
			}
			fmt.Fprintf(tw, "%s%s\t%s\n", voice.ID, mark, voice.Language)
		}
		return tw.Flush()
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s\t%s\n", k, v)
		}
	}
	row("Model", m.Ref())
	row("Description", m.Description)
	row("Task", m.Task)
	row("Languages", strings.Join(m.Languages, ", "))
	row("Capabilities", strings.Join(m.Capabilities, ", "))
	row("Sample rate", fmt.Sprintf("%d Hz", m.Audio.SampleRate))
	fmt.Fprintln(tw, "\t")
	row("Original model", m.Upstream.Model)
	row("Authors", m.Upstream.Authors)
	row("Model page", m.Upstream.URL)
	row("Engine", m.Engine)
	fmt.Fprintln(tw, "\t")
	row("License", licenseLabel(m.License))
	row("License URL", m.License.URL)
	row("License notes", m.License.Notes)
	for _, c := range m.License.Components {
		row("  includes", fmt.Sprintf("%s under %s. %s", c.Name, c.SPDX, c.Note))
	}
	fmt.Fprintln(tw, "\t")
	if m.Source.Type == manifest.SourceSystem {
		row("Source", "provided by the operating system")
	} else {
		row("Source", fmt.Sprintf("%s/%s @ %s", pull.Endpoint(), m.Source.Repo, m.Source.Revision))
		row("Files", fmt.Sprintf("%d, %s, stored unmodified", len(m.Source.Files), humanSize(m.TotalSize())))
		status := "not installed"
		if info.Installed {
			status = info.Path
		}
		row("Installed", status)
	}
	if len(m.Voices) > 0 {
		row("Voices", fmt.Sprintf("%d (default %s); list with --voices", len(m.Voices), m.DefaultVoice))
	}
	if !info.Supported {
		row("Warning", "this model does not run on "+runtime.GOOS+"/"+runtime.GOARCH)
	}
	row("Warning", info.EngineError)
	return tw.Flush()
}

func cmdStop(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: oaudio stop <model>")
	}
	if err := ensureServer(); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"model": args[0]})
	resp, err := http.Post(baseURL()+"/api/stop", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return nil
}

func cmdRm(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: oaudio rm <model>")
	}
	if err := ensureServer(); err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodDelete, baseURL()+"/api/models?model="+url.QueryEscape(args[0]), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	fmt.Println("deleted", args[0])
	return nil
}

func cmdVerify(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: oaudio verify <model>")
	}
	if err := ensureServer(); err != nil {
		return err
	}
	info, err := show(args[0])
	if err != nil {
		return err
	}
	if info.Manifest.Source.Type == manifest.SourceSystem {
		return errors.New("this model is provided by the operating system; there are no files to verify")
	}
	if !info.Installed {
		return fmt.Errorf("%s is not installed", info.Manifest.Ref())
	}
	st := store.New(store.DefaultHome())
	if err := st.Verify(&info.Manifest); err != nil {
		return err
	}
	fmt.Printf("%s: %d files verified against the hashes published upstream\n", info.Manifest.Ref(), st.FileCount(&info.Manifest))
	return nil
}

func cmdRun(args []string) error {
	opts, pos, err := parseArgs(args, map[string]bool{"words": true, "diarize": true}, map[string]string{"o": "output", "f": "format", "v": "voice", "l": "language"})
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return errors.New("usage: oaudio run <model> [text | audio-file] [--voice V] [--language L] [--format F] [-o file]")
	}
	if err := ensureServer(); err != nil {
		return err
	}
	info, err := show(pos[0])
	if err != nil {
		return err
	}
	if !info.Installed {
		if err := pullModel(info.Manifest.Ref()); err != nil {
			return err
		}
	}
	switch info.Manifest.Task {
	case manifest.TaskTTS:
		return runSpeak(info.Manifest.Ref(), pos[1:], opts)
	case manifest.TaskSTT:
		return runTranscribe(info.Manifest.Ref(), pos[1:], opts)
	case manifest.TaskVAD:
		return runAnalyze("/v1/audio/vad", info.Manifest.Ref(), pos[1:], opts, []string{"threshold", "min_silence_ms", "min_speech_ms"})
	case manifest.TaskDiarize:
		return runAnalyze("/v1/audio/diarizations", info.Manifest.Ref(), pos[1:], opts, []string{"num_speakers", "speaker_threshold"})
	}
	return fmt.Errorf("unknown task %q", info.Manifest.Task)
}

func runSpeak(model string, pos []string, opts map[string]string) error {
	speak := func(text string) error {
		req := map[string]any{"model": model, "input": text}
		if v := opts["voice"]; v != "" {
			req["voice"] = v
		}
		if v := opts["language"]; v != "" {
			req["language"] = v
		}
		if v := opts["speed"]; v != "" {
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
				return fmt.Errorf("invalid --speed %q", v)
			}
			req["speed"] = f
		}
		out := opts["output"]
		format := opts["format"]
		if format == "" {
			format = "wav"
			if ext := strings.TrimPrefix(filepath.Ext(out), "."); ext != "" && out != "-" {
				format = ext
			}
		}
		req["response_format"] = format
		body, _ := json.Marshal(req)
		resp, err := http.Post(baseURL()+"/v1/audio/speech", "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return apiError(resp)
		}
		stdoutIsTerminal := false
		if st, err := os.Stdout.Stat(); err == nil {
			stdoutIsTerminal = st.Mode()&os.ModeCharDevice != 0
		}
		switch {
		case out == "-" || (out == "" && !stdoutIsTerminal):
			_, err = io.Copy(os.Stdout, resp.Body)
			return err
		case out != "":
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, resp.Body)
			if err != nil {
				f.Close()
				return err
			}
			// A streamed WAV does not know its own length. Now that the file
			// is complete, write the real sizes so every tool can read it.
			if format == "wav" && n >= 44 && n-8 <= 0xFFFFFFFF {
				var size [4]byte
				binary.LittleEndian.PutUint32(size[:], uint32(n-8))
				f.WriteAt(size[:], 4)
				binary.LittleEndian.PutUint32(size[:], uint32(n-44))
				f.WriteAt(size[:], 40)
			}
			return f.Close()
		}
		return play(resp.Body)
	}

	if len(pos) > 0 {
		return speak(strings.Join(pos, " "))
	}
	// No text argument: read stdin. Piped input is spoken as a whole so the
	// server can stream it sentence by sentence; a terminal is read line by line.
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(data)) == "" {
			return errors.New("no text to speak")
		}
		return speak(string(data))
	}
	sc := bufio.NewScanner(os.Stdin)
	fmt.Fprint(os.Stderr, ">>> ")
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			if err := speak(line); err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
			}
		}
		fmt.Fprint(os.Stderr, ">>> ")
	}
	fmt.Fprintln(os.Stderr)
	return sc.Err()
}

// play sends a WAV stream to the speakers. ffplay plays while the stream is
// still arriving; afplay (always present on macOS) needs the whole file.
func play(wav io.Reader) error {
	if p, err := exec.LookPath("ffplay"); err == nil {
		cmd := exec.Command(p, "-nodisp", "-autoexit", "-loglevel", "error", "-")
		cmd.Stdin, cmd.Stderr = wav, os.Stderr
		return cmd.Run()
	}
	tmp, err := os.CreateTemp("", "oaudio-*.wav")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, wav); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	for _, player := range []string{"afplay", "aplay", "paplay"} {
		if p, err := exec.LookPath(player); err == nil {
			return exec.Command(p, tmp.Name()).Run()
		}
	}
	return errors.New("no audio player found (tried ffplay, afplay, aplay, paplay); use -o to save a file instead")
}

func runTranscribe(model string, pos []string, opts map[string]string) error {
	if len(pos) != 1 {
		return errors.New("usage: oaudio run <stt-model> <audio-file> [--language L] [--format text|json|srt|vtt|verbose_json] [--words]")
	}
	f, err := os.Open(pos[0])
	if err != nil {
		return err
	}
	defer f.Close()
	format := opts["format"]
	if format == "" {
		format = "text"
	}
	if opts["words"] != "" {
		format = "verbose_json"
	}
	diarize := opts["diarize"] != "" || opts["diarization-model"] != ""
	if diarize {
		format = "diarized_json"
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fields := map[string]string{"model": model, "response_format": format, "language": opts["language"], "prompt": opts["prompt"],
			"diarization_model": opts["diarization-model"], "num_speakers": opts["speakers"]}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if fields[k] != "" {
				mw.WriteField(k, fields[k])
			}
		}
		if opts["words"] != "" {
			mw.WriteField("timestamp_granularities[]", "word")
			mw.WriteField("timestamp_granularities[]", "segment")
		}
		part, err := mw.CreateFormFile("file", filepath.Base(pos[0]))
		if err == nil {
			_, err = io.Copy(part, f)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL()+"/v1/audio/transcriptions", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	out := io.Writer(os.Stdout)
	if o := opts["output"]; o != "" && o != "-" {
		file, err := os.Create(o)
		if err != nil {
			return err
		}
		defer file.Close()
		out = file
	}
	if diarize && opts["format"] == "" {
		// Show a diarized transcript as a readable script.
		var d struct {
			Segments []struct {
				Start, End    float64
				Speaker, Text string
			} `json:"segments"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			return err
		}
		for _, seg := range d.Segments {
			fmt.Fprintf(out, "[%s - %s] %s: %s\n", clock(seg.Start), clock(seg.End), seg.Speaker, seg.Text)
		}
		return nil
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

func clock(sec float64) string {
	t := int(sec*10 + 0.5)
	return fmt.Sprintf("%02d:%02d.%d", t/600, t/10%60, t%10)
}

// postFile uploads one file with form fields and returns the response.
func postFile(path, fileField, filePath string, fields map[string]string) (*http.Response, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if fields[k] != "" {
			mw.WriteField(k, fields[k])
		}
	}
	part, err := mw.CreateFormFile(fileField, filepath.Base(filePath))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, err
	}
	mw.Close()
	resp, err := http.Post(baseURL()+path, mw.FormDataContentType(), &body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp, nil
}

// runAnalyze serves the models that describe a recording rather than
// transcribe it: where the speech is, and who is speaking.
func runAnalyze(path, model string, pos []string, opts map[string]string, pass []string) error {
	if len(pos) != 1 {
		return errors.New("usage: oaudio run <model> <audio-file> [--format json]")
	}
	fields := map[string]string{"model": model}
	for _, k := range pass {
		fields[k] = opts[strings.ReplaceAll(k, "_", "-")]
	}
	resp, err := postFile(path, "file", pos[0], fields)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if opts["format"] == "json" {
		_, err = io.Copy(os.Stdout, resp.Body)
		return err
	}
	var out struct {
		Duration       float64 `json:"duration"`
		SpeechDuration float64 `json:"speech_duration"`
		Speakers       []string
		Segments       []struct {
			Start, End float64
			Speaker    string
		} `json:"segments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	for _, seg := range out.Segments {
		if seg.Speaker != "" {
			fmt.Printf("%s - %s  %s\n", clock(seg.Start), clock(seg.End), seg.Speaker)
		} else {
			fmt.Printf("%s - %s\n", clock(seg.Start), clock(seg.End))
		}
	}
	if out.Speakers != nil {
		fmt.Fprintf(os.Stderr, "%d speakers in %s of audio\n", len(out.Speakers), clock(out.Duration))
	} else {
		fmt.Fprintf(os.Stderr, "%s of speech in %s of audio\n", clock(out.SpeechDuration), clock(out.Duration))
	}
	return nil
}

type voiceInfo struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  int64   `json:"created_at"`
	Consent    string  `json:"consent"`
	Transcript string  `json:"transcript"`
	Seconds    float64 `json:"seconds"`
}

func cmdVoice(args []string) error {
	const usage = `usage:
  oaudio voice create <name> --audio <file> --consent <who agreed> [--transcript <text> | --transcribe-with <stt-model>] [--language L]
  oaudio voice list
  oaudio voice rm <name-or-id>`
	if len(args) == 0 {
		return errors.New(usage)
	}
	opts, pos, err := parseArgs(args[1:], nil, nil)
	if err != nil {
		return err
	}
	if err := ensureServer(); err != nil {
		return err
	}
	switch args[0] {
	case "create":
		if len(pos) != 1 || opts["audio"] == "" {
			return errors.New(usage)
		}
		if opts["consent"] == "" {
			return errors.New("--consent is required: say who agreed to have their voice copied, for example --consent \"my own voice\"")
		}
		resp, err := postFile("/v1/audio/voices", "audio_sample", opts["audio"], map[string]string{
			"name": pos[0], "consent": opts["consent"], "transcript": opts["transcript"],
			"transcription_model": opts["transcribe-with"], "language": opts["language"],
		})
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var v voiceInfo
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			return err
		}
		fmt.Printf("created voice %q (%s, %.1f s)\n", v.Name, v.ID, v.Seconds)
		if v.Transcript == "" {
			fmt.Println("note: no transcript was given; models that need one will refuse this voice")
		} else if opts["transcript"] == "" {
			fmt.Printf("transcript: %s\n", v.Transcript)
		}
		return nil
	case "list", "ls":
		var out struct {
			Data []voiceInfo `json:"data"`
		}
		if err := getJSON("/v1/audio/voices", &out); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "NAME\tID\tLENGTH\tCREATED\tCONSENT")
		for _, v := range out.Data {
			fmt.Fprintf(tw, "%s\t%s\t%.1f s\t%s\t%s\n", v.Name, v.ID, v.Seconds, time.Unix(v.CreatedAt, 0).Format("2006-01-02"), v.Consent)
		}
		return tw.Flush()
	case "rm":
		if len(pos) != 1 {
			return errors.New(usage)
		}
		req, _ := http.NewRequest(http.MethodDelete, baseURL()+"/v1/audio/voices/"+url.PathEscape(pos[0]), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return apiError(resp)
		}
		fmt.Println("deleted voice", pos[0])
		return nil
	}
	return errors.New(usage)
}

// cmdManifest is a maintainer tool: it prints a manifest skeleton for a
// Hugging Face repo with the hashes the Hub publishes, ready for review.
func cmdManifest(args []string) error {
	opts, pos, err := parseArgs(args, nil, nil)
	if err != nil {
		return err
	}
	if len(pos) != 2 || pos[0] != "hf" {
		return errors.New("usage: oaudio manifest hf <repo> [--revision main] [--include glob,glob] [--name N --tag T --task stt|tts --engine E]")
	}
	rev := opts["revision"]
	if rev == "" {
		rev = "main"
	}
	var globs []string
	if g := opts["include"]; g != "" {
		globs = strings.Split(g, ",")
	}
	include := func(p string) bool {
		if len(globs) == 0 {
			return true
		}
		for _, g := range globs {
			if ok, _ := filepath.Match(g, p); ok {
				return true
			}
			if strings.HasSuffix(g, "/**") && strings.HasPrefix(p, strings.TrimSuffix(g, "**")) {
				return true
			}
		}
		return false
	}
	commit, files, err := pull.HFFiles(context.Background(), pos[1], rev, include)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no files matched")
	}
	m := manifest.Manifest{
		Schema: manifest.Schema, Name: opts["name"], Tag: opts["tag"], Task: opts["task"], Engine: opts["engine"],
		Source: manifest.Source{Type: manifest.SourceHuggingFace, Repo: pos[1], Revision: commit, Files: files},
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

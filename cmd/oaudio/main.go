// Command oaudio runs speech models locally.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eastsidegunn/oaudio/internal/catalog"
	"github.com/eastsidegunn/oaudio/internal/engines/apple"
	"github.com/eastsidegunn/oaudio/internal/engines/ref"
	"github.com/eastsidegunn/oaudio/internal/server"
	"github.com/eastsidegunn/oaudio/internal/store"
	"github.com/eastsidegunn/oaudio/runner"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0-dev"

const usage = `Oaudio runs speech models locally.

Usage:
  oaudio <command> [arguments]

Commands:
  serve                 Start the server (default http://127.0.0.1:11535)
  pull <model>          Download a model and verify it against upstream hashes
  run <model> [input]   Speak text, or transcribe / find speech / find speakers in a file
  voice <subcommand>    Manage custom voices for cloning: create, list, rm
  list                  List models ready to run
  catalog               List every model Oaudio knows how to run
  show <model>          Show a model's origin, license, files and voices
  ps                    List models loaded in memory
  stop <model>          Unload a model from memory
  rm <model>            Delete a downloaded model
  verify <model>        Re-check a downloaded model against upstream hashes
  version               Print the version

Examples:
  oaudio run apple-say "Hello from Oaudio."
  oaudio run apple-say --voice Yuna -o hello.wav "안녕하세요."
  oaudio run apple-speech meeting.wav --format srt
  echo "Read this aloud." | oaudio run apple-say
  oaudio run whisper call.wav --diarize            # who said what
  oaudio run silero-vad call.wav                   # where the speech is
  oaudio voice create anna --audio anna.wav --transcript "What she says in it." --consent "Anna agreed, 2026-11-11"
  oaudio run zipvoice --voice anna "Spoken in Anna's voice."

Environment:
  OAUDIO_HOST        address to listen on / connect to (default 127.0.0.1:11535)
  OAUDIO_HOME        where models live (default ~/.oaudio)
  OAUDIO_KEEP_ALIVE  how long idle models stay loaded (default 5m; -1 forever)
  OAUDIO_MAX_LOADED  how many models may be loaded at once (default 3)
  OAUDIO_ALIASES     model name aliases, e.g. "tts-1=apple-say,whisper-1=apple-speech"
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe()
	case "runner":
		err = cmdRunner(args)
	case "pull":
		err = cmdPull(args)
	case "run":
		err = cmdRun(args)
	case "list", "ls":
		err = cmdList(false)
	case "catalog":
		err = cmdList(true)
	case "show":
		err = cmdShow(args)
	case "ps":
		err = cmdPs()
	case "stop":
		err = cmdStop(args)
	case "rm":
		err = cmdRm(args)
	case "verify":
		err = cmdVerify(args)
	case "voice":
		err = cmdVoice(args)
	case "manifest":
		err = cmdManifest(args)
	case "version", "--version", "-v":
		fmt.Println("oaudio", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "oaudio: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// cmdRunner is the hidden entry point for engines compiled into this binary.
func cmdRunner(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: oaudio runner <engine> --manifest <file> --model-dir <dir>")
	}
	switch args[0] {
	case "apple":
		runner.Main(args[1:], apple.New)
	case "reference":
		runner.Main(args[1:], ref.New)
	default:
		return fmt.Errorf("no built-in engine %q", args[0])
	}
	return nil
}

func cmdServe() error {
	st := store.New(store.DefaultHome())
	if err := os.MkdirAll(st.Home, 0o755); err != nil {
		return err
	}
	cat, err := catalog.Load(st.ManifestsDir())
	if err != nil {
		return err
	}
	cfg := server.Config{
		Version: version,
		Store:   st,
		Catalog: cat,
		Aliases: map[string]string{},
		Log:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	if v := os.Getenv("OAUDIO_KEEP_ALIVE"); v != "" {
		d, err := server.ParseKeepAlive(v)
		if err != nil {
			return err
		}
		cfg.KeepAlive = *d
		if cfg.KeepAlive == 0 {
			cfg.KeepAlive = time.Nanosecond // "0" means unload right away, not "use the default"
		}
	}
	if v := os.Getenv("OAUDIO_MAX_LOADED"); v != "" {
		if cfg.MaxLoaded, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("OAUDIO_MAX_LOADED: %w", err)
		}
	}
	for _, pair := range strings.Split(os.Getenv("OAUDIO_ALIASES"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			cfg.Aliases[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	srv := server.New(cfg)
	defer srv.Close()

	addr := server.Addr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w (is oaudio already running?)", addr, err)
	}
	httpSrv := &http.Server{Handler: srv.Handler()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	cfg.Log.Info("oaudio is listening", "addr", "http://"+ln.Addr().String(), "version", version, "home", st.Home)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdown)
	return nil
}

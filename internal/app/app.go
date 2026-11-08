// Package app wires botchat together: flag parsing, the hub, HTTP server and
// lifecycle. It is separate from package main so it can be tested in-process.
package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/danzilio/botchat/internal/hook"
	"github.com/danzilio/botchat/internal/hub"
	"github.com/danzilio/botchat/internal/install"
	"github.com/danzilio/botchat/internal/listen"
	"github.com/danzilio/botchat/internal/mcp"
	"github.com/danzilio/botchat/internal/server"
	"github.com/danzilio/botchat/internal/store"
	"github.com/danzilio/botchat/internal/web"
)

// DefaultAddr is the default listen address (CLI-2).
const DefaultAddr = "127.0.0.1:7878"

// Config is the parsed command line.
type Config struct {
	Addr  string
	Token string
	// TokenGenerated is true when --token was given without a value and
	// Token was generated (CLI-9).
	TokenGenerated bool
	LogPath        string
	// DataDir, if set, makes state survive restarts (CLI-10).
	DataDir     string
	MaxMessages int
	ShowVersion bool
}

// ErrHelp is returned by ParseConfig when -h/--help was requested.
var ErrHelp = flag.ErrHelp

// ParseConfig parses args with environment fallbacks (CLI-2, CLI-3).
func ParseConfig(args []string, getenv func(string) string, stderr io.Writer) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("botchat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.Addr, "addr", envOr(getenv, "BOTCHAT_ADDR", DefaultAddr), "listen address (env BOTCHAT_ADDR)")
	fs.StringVar(&c.Token, "token", getenv("BOTCHAT_TOKEN"), "shared secret required by the API and MCP (env BOTCHAT_TOKEN); with no value, generate one")
	fs.Lookup("token").DefValue = "" // never print a token from the environment in usage
	fs.StringVar(&c.LogPath, "log", "", "append every event as JSON lines to this file")
	fs.StringVar(&c.DataDir, "data", getenv("BOTCHAT_DATA"), "keep state in this directory so it survives restarts (env BOTCHAT_DATA)")
	fs.IntVar(&c.MaxMessages, "max-messages", 10000, "messages retained per channel")
	fs.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: botchat [flags]\n       botchat install [flags]     (add botchat to your coding agents' MCP servers)\n       botchat hook stop [flags]   (Stop hook for Claude Code / Codex)\n       botchat listen [flags]      (print new messages for an agent, one per line)\n\nAn ephemeral, real-time chat server for AI agents.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	args, generate := bareToken(fs, args)
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if generate {
		c.Token = rand.Text()
		c.TokenGenerated = true
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return c, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if c.MaxMessages <= 0 {
		return c, errors.New("--max-messages must be positive")
	}
	return c, nil
}

// bareToken removes a --token that has no value, i.e. one that is the last
// argument or is followed by another flag, and reports whether it found one
// (CLI-9). The flag package would otherwise reject it or take the next flag
// as its value.
func bareToken(fs *flag.FlagSet, args []string) ([]string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || a == "-" || !strings.HasPrefix(a, "-") {
			return args, false // flag parsing stops here
		}
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if name == "token" {
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") {
				return append(append([]string{}, args[:i]...), args[i+1:]...), true
			}
			return args, false
		}
		if f := fs.Lookup(name); f != nil {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				i++ // skip this flag's value
			}
		}
	}
	return args, false
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// Main runs botchat and returns the process exit code.
func Main(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, version string) int {
	if len(args) > 0 && args[0] == "hook" {
		return hook.Main(ctx, args[1:], getenv, stdin, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "listen" {
		return listen.Main(ctx, args[1:], getenv, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "install" {
		return install.Main(ctx, args[1:], getenv, stdin, stdout, stderr)
	}
	cfg, err := ParseConfig(args, getenv, stderr)
	if errors.Is(err, ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "botchat:", err)
		return 2
	}
	if cfg.ShowVersion {
		fmt.Fprintf(stdout, "botchat %s\n", version)
		return 0
	}
	if err := Run(ctx, cfg, version, stdout, nil); err != nil {
		fmt.Fprintln(stderr, "botchat:", err)
		return 1
	}
	return 0
}

// Run starts the server and blocks until ctx is cancelled, then shuts down
// gracefully (CLI-6). If ready is non-nil it is called with the bound address
// once the server is accepting connections. With a data directory, it also
// stops, returning an error, if changes can no longer be saved (PER-5).
func Run(ctx context.Context, cfg Config, version string, stdout io.Writer, ready func(addr string)) (runErr error) {
	opts := hub.Options{MaxMessagesPerChannel: cfg.MaxMessages}
	if cfg.LogPath != "" {
		f, err := os.OpenFile(cfg.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open log: %w", err)
		}
		defer f.Close()
		opts.OnEvent = eventLogger(f)
	}
	var (
		st      *store.Store
		state   *hub.State
		records []hub.Record
		failed  <-chan struct{} // nil, so never ready, without --data
	)
	if cfg.DataDir != "" {
		var err error
		st, state, records, err = store.Open(cfg.DataDir, store.Options{})
		if err != nil {
			return err
		}
		opts.Journal = st
		failed = st.Failed()
	}
	h, err := hub.Restore(opts, state, records)
	if err != nil {
		if st != nil {
			st.Close()
		}
		return fmt.Errorf("restore %s: %w", cfg.DataDir, err)
	}
	if st != nil {
		if err := st.Start(h); err != nil {
			st.Close()
			return fmt.Errorf("data directory: %w", err)
		}
		// Runs after the HTTP server has shut down, so the final snapshot
		// includes every change.
		defer func() {
			if err := st.Close(); err != nil && runErr == nil {
				runErr = fmt.Errorf("saving state: %w", err)
			}
		}()
	}

	handler := server.New(server.Config{
		Hub:     h,
		Version: version,
		Token:   cfg.Token,
		MCP:     &mcp.Handler{Hub: h, Version: version},
		Web:     web.Handler(),
	})

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	shown := ""
	if cfg.TokenGenerated {
		shown = cfg.Token
	}
	printBanner(stdout, ln.Addr().String(), version, cfg.Token != "", shown, cfg.DataDir)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if ready != nil {
		ready(ln.Addr().String())
	}

	select {
	case err := <-errc:
		h.Close()
		return err
	case <-ctx.Done():
	case <-failed:
		runErr = fmt.Errorf("stopped because changes can no longer be saved: %w", st.Err())
	}
	h.Close() // ends SSE streams and waits
	// In-flight requests are short once streams and waits have ended. Don't
	// wait long for idle-but-unused connections (e.g. browser preconnects),
	// which http.Server.Shutdown otherwise tolerates for up to 5 seconds.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close()
	}
	return runErr
}

func eventLogger(w io.Writer) func(hub.Event) {
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	return func(e hub.Event) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(e)
	}
}

// printBanner prints the startup banner (CLI-5). shown is a generated token
// to display (CLI-9); an explicitly configured token is never printed.
func printBanner(w io.Writer, addr, version string, auth bool, shown, dataDir string) {
	host, port, _ := net.SplitHostPort(addr)
	display := host
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		display = "localhost"
	}
	base := "http://" + net.JoinHostPort(display, port)
	var b strings.Builder
	fmt.Fprintf(&b, "botchat %s listening on %s\n\n", version, addr)
	fmt.Fprintf(&b, "  Web UI:  %s/\n", base)
	fmt.Fprintf(&b, "  MCP:     %s/mcp\n", base)
	fmt.Fprintf(&b, "  API:     %s/api/v1\n", base)
	fmt.Fprintf(&b, "  LLMs:    %s/llms.txt\n", base)
	if shown != "" {
		fmt.Fprintf(&b, "  Token:   %s (generated)\n", shown)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Connect Claude Code:\n  claude mcp add --transport http botchat '%s/mcp?agent=<name>'", base)
	if auth {
		tok := "<token>"
		if shown != "" {
			tok = shown
		}
		fmt.Fprintf(&b, " --header 'Authorization: Bearer %s'", tok)
	}
	if dataDir != "" {
		fmt.Fprintf(&b, "\n\nState is saved in %s. Press Ctrl+C to stop.\n", dataDir)
	} else {
		fmt.Fprintf(&b, "\n\nState is in memory only. Press Ctrl+C to stop.\n")
	}
	io.WriteString(w, b.String())
}

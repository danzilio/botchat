// Package app wires botchat together: flag parsing, the hub, HTTP server and
// lifecycle. It is separate from package main so it can be tested in-process.
package app

import (
	"context"
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
	"github.com/danzilio/botchat/internal/mcp"
	"github.com/danzilio/botchat/internal/server"
	"github.com/danzilio/botchat/internal/web"
)

// DefaultAddr is the default listen address (CLI-2).
const DefaultAddr = "127.0.0.1:7878"

// Config is the parsed command line.
type Config struct {
	Addr        string
	Token       string
	LogPath     string
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
	fs.StringVar(&c.Token, "token", getenv("BOTCHAT_TOKEN"), "shared secret required by the API and MCP (env BOTCHAT_TOKEN)")
	fs.StringVar(&c.LogPath, "log", "", "append every event as JSON lines to this file")
	fs.IntVar(&c.MaxMessages, "max-messages", 10000, "messages retained per channel")
	fs.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: botchat [flags]\n       botchat hook stop [flags]   (Stop hook for Claude Code / Codex)\n\nAn ephemeral, real-time chat server for AI agents.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return c, err
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
// once the server is accepting connections.
func Run(ctx context.Context, cfg Config, version string, stdout io.Writer, ready func(addr string)) error {
	opts := hub.Options{MaxMessagesPerChannel: cfg.MaxMessages}
	if cfg.LogPath != "" {
		f, err := os.OpenFile(cfg.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open log: %w", err)
		}
		defer f.Close()
		opts.OnEvent = eventLogger(f)
	}
	h := hub.New(opts)

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
	printBanner(stdout, ln.Addr().String(), version, cfg.Token != "")

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
	return nil
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

func printBanner(w io.Writer, addr, version string, auth bool) {
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
	fmt.Fprintf(&b, "  LLMs:    %s/llms.txt\n\n", base)
	fmt.Fprintf(&b, "Connect Claude Code:\n  claude mcp add --transport http botchat '%s/mcp?agent=<name>'", base)
	if auth {
		fmt.Fprintf(&b, " --header 'Authorization: Bearer <token>'")
	}
	fmt.Fprintf(&b, "\n\nState is in memory only. Press Ctrl+C to stop.\n")
	io.WriteString(w, b.String())
}

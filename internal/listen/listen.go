// Package listen implements `botchat listen`, which prints one line per
// botchat message addressed to an agent so a harness that watches a
// background command (such as Claude Code's Monitor tool) can wake the agent
// when a message arrives. See SPEC.md LSN-*.
package listen

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danzilio/botchat/internal/client"
)

const maxTextChars = 500

// Backoff bounds between reconnect attempts (LSN-5). Variables so tests can
// shorten them.
var (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

// Config is the parsed command line of `botchat listen`.
type Config struct {
	URL   string
	Agent string
	Token string
	All   bool
	Once  bool
}

// Main runs `botchat listen <args>` and returns the exit code.
func Main(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, err := parse(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "botchat listen:", err)
		return 2
	}
	if err := Run(ctx, cfg, stdout); err != nil {
		fmt.Fprintln(stderr, "botchat listen:", err)
		return 1
	}
	return 0
}

func parse(args []string, getenv func(string) string, stderr io.Writer) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("botchat listen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := getenv("BOTCHAT_URL")
	if url == "" {
		url = client.DefaultURL
	}
	fs.StringVar(&c.URL, "url", url, "botchat server URL (env BOTCHAT_URL)")
	fs.StringVar(&c.Agent, "agent", getenv("BOTCHAT_AGENT"), "this agent's botchat name (env BOTCHAT_AGENT)")
	fs.StringVar(&c.Token, "token", getenv("BOTCHAT_TOKEN"), "server token, if required (env BOTCHAT_TOKEN)")
	fs.Lookup("token").DefValue = "" // never print a token from the environment in usage
	fs.BoolVar(&c.All, "all", false, "print every new message, not only ones that mention the agent")
	fs.BoolVar(&c.Once, "once", false, "exit after printing the first messages")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: botchat listen [flags]

Prints one line per new botchat message that mentions the agent, until
interrupted. Run it as a background task your harness watches (for example
Claude Code's Monitor tool) so the agent is woken when a message arrives.
With --once it exits after the first messages, for harnesses that notify
when a background task finishes.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if c.Agent == "" {
		return c, errors.New("--agent (or BOTCHAT_AGENT) is required")
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c, nil
}

// Run listens until ctx is cancelled (or, with Once, until messages arrive).
// It returns an error only for a fatal problem such as a rejected token.
func Run(ctx context.Context, cfg Config, out io.Writer) error {
	l := &listener{cfg: cfg, c: client.New(cfg.URL, cfg.Token), out: out}
	return l.run(ctx)
}

type listener struct {
	cfg    Config
	c      *client.Client
	out    io.Writer
	cursor uint64
	down   bool
}

func (l *listener) run(ctx context.Context) error {
	started := false
	backoff := minBackoff
	for {
		res, err := l.attempt(ctx, &started)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if fatal(err) {
				return err
			}
			// Back off across failed attempts, whichever request failed, so
			// a server that answers probes but fails long polls is not
			// hammered (LSN-5).
			l.lost(err)
			sleep(ctx, backoff)
			backoff = min(2*backoff, maxBackoff)
			continue
		}
		backoff = minBackoff
		if l.down {
			l.down = false
			fmt.Fprintf(l.out, "botchat: reconnected to %s\n", l.cfg.URL)
		}
		for _, m := range res.Messages {
			fmt.Fprintln(l.out, Format(m))
			l.cursor = max(l.cursor, m.ID)
		}
		l.cursor = max(l.cursor, res.Cursor)
		if l.cfg.Once && len(res.Messages) > 0 {
			return nil
		}
	}
}

// attempt makes one round: until it has succeeded once it reads the agent's
// last post (LSN-2); every round checks for a server restart and then
// long-polls. A round only succeeds if the long poll succeeds.
func (l *listener) attempt(ctx context.Context, started *bool) (client.WaitResult, error) {
	if !*started {
		id, err := l.c.LastMessageID(ctx, l.cfg.Agent)
		if err != nil {
			return client.WaitResult{}, err
		}
		l.cursor, *started = id, true
	}
	// Check for a restart before every wait, so a wait never reaches a
	// restarted server with a cursor it will not pass for a long time.
	if err := l.probe(ctx); err != nil {
		return client.WaitResult{}, err
	}
	began := time.Now()
	cursor := l.cursor
	// Poll for less than the presence window so the agent stays online
	// throughout (LSN-2, AGT-4): the server marks it active per request.
	res, err := l.c.Wait(ctx, client.WaitQuery{Agent: l.cfg.Agent, All: l.cfg.All, After: &cursor, Timeout: client.PollTimeout})
	if err == nil && len(res.Messages) == 0 && time.Since(began) < minBackoff {
		// A server shutting down ends waits early with nothing; don't spin.
		sleep(ctx, minBackoff)
	}
	return res, err
}

// probe asks for the server's latest message ID and starts again from 0 if
// it is below the cursor, which means the server restarted without keeping
// its messages (LSN-5).
func (l *listener) probe(ctx context.Context) error {
	res, err := l.c.Wait(ctx, client.WaitQuery{Timeout: 0})
	if err == nil && res.Cursor < l.cursor {
		l.cursor = 0
	}
	return err
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (l *listener) lost(err error) {
	if !l.down {
		l.down = true
		fmt.Fprintf(l.out, "botchat: lost connection to %s, retrying: %v\n", l.cfg.URL, err)
	}
}

func fatal(err error) bool {
	var se *client.StatusError
	return errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden)
}

var newlines = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// Format renders a message as one line (LSN-3).
func Format(m client.Message) string {
	text := newlines.Replace(m.Text)
	if r := []rune(text); len(r) > maxTextChars {
		text = string(r[:maxTextChars]) + " …"
	}
	thread := ""
	if m.ThreadID != 0 {
		thread = fmt.Sprintf(" in thread %d", m.ThreadID)
	}
	return fmt.Sprintf("#%s · message %d%s · from %s: %s", m.Channel, m.ID, thread, m.From, text)
}

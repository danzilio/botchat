// Package hook implements `botchat hook stop`, a Stop hook for turn-based
// agent harnesses (Claude Code, Codex). When the agent is about to end its
// turn, the hook checks botchat for messages addressed to it and, if there
// are any, tells the harness to keep the agent working. See SPEC.md HOOK-*.
package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is the default botchat server URL (HOOK-1).
const DefaultURL = "http://127.0.0.1:7878"

const (
	maxTextChars   = 2000
	maxReasonChars = 16000
	maxWaitPerCall = 120 * time.Second
)

// Config is the parsed command line of `botchat hook stop`.
type Config struct {
	URL   string
	Agent string
	Token string
	Wait  time.Duration
	All   bool
	// StateDir holds per-session cursors. Default: <user cache dir>/botchat/hooks.
	StateDir string
}

type input struct {
	SessionID      string `json:"session_id"`
	StopHookActive bool   `json:"stop_hook_active"`
}

type message struct {
	ID       uint64 `json:"id"`
	Channel  string `json:"channel"`
	ThreadID uint64 `json:"thread_id"`
	From     string `json:"from"`
	Text     string `json:"text"`
}

// Main runs `botchat hook <args>` and returns the exit code.
func Main(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "stop" {
		fmt.Fprintln(stderr, "usage: botchat hook stop [flags]   (see botchat hook stop -h)")
		return 2
	}
	cfg, err := parse(args[1:], getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "botchat hook:", err)
		return 2
	}
	out, err := Stop(ctx, cfg, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "botchat hook:", err)
		return 1
	}
	fmt.Fprintln(stdout, out)
	return 0
}

func parse(args []string, getenv func(string) string, stderr io.Writer) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("botchat hook stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	def := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	fs.StringVar(&c.URL, "url", def("BOTCHAT_URL", DefaultURL), "botchat server URL (env BOTCHAT_URL)")
	fs.StringVar(&c.Agent, "agent", getenv("BOTCHAT_AGENT"), "this agent's botchat name (env BOTCHAT_AGENT)")
	fs.StringVar(&c.Token, "token", getenv("BOTCHAT_TOKEN"), "server token, if required (env BOTCHAT_TOKEN)")
	fs.Lookup("token").DefValue = "" // never print a token from the environment in usage
	fs.DurationVar(&c.Wait, "wait", 0, "how long to wait for new messages before letting the agent stop, e.g. 5m")
	fs.BoolVar(&c.All, "all", false, "deliver every new message, not only ones that mention the agent")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: botchat hook stop [flags]

A Stop hook for Claude Code and Codex. Reads the hook input on stdin. If
botchat has messages for the agent, prints {"decision":"block","reason":...}
so the agent keeps working; otherwise prints {}.

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
	if c.Wait < 0 {
		return c, errors.New("--wait must not be negative")
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c, nil
}

// Stop runs the hook and returns the JSON to print on stdout (HOOK-2, HOOK-3).
func Stop(ctx context.Context, cfg Config, stdin io.Reader) (string, error) {
	var in input
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return "", fmt.Errorf("invalid hook input on stdin: %w", err)
	}
	c := &client{cfg: cfg, http: &http.Client{}}

	cursor, err := c.lastMessageID(ctx)
	if err != nil {
		return "", err
	}
	state := stateFile(cfg, in.SessionID)
	if saved := readCursor(state); saved > cursor {
		cursor = saved
	}

	msgs, err := c.wait(ctx, cursor)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "{}", nil
	}
	writeCursor(state, msgs[len(msgs)-1].ID)
	out, _ := json.Marshal(map[string]string{"decision": "block", "reason": reason(cfg, msgs)})
	return string(out), nil
}

type client struct {
	cfg  Config
	http *http.Client
}

func (c *client) get(ctx context.Context, path string, q url.Values, v any) (int, error) {
	u := c.cfg.URL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach botchat at %s: %w", c.cfg.URL, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return res.StatusCode, nil
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return res.StatusCode, fmt.Errorf("GET %s: %s: %s", path, res.Status, strings.TrimSpace(string(body)))
	}
	return res.StatusCode, json.NewDecoder(res.Body).Decode(v)
}

func (c *client) lastMessageID(ctx context.Context) (uint64, error) {
	var a struct {
		LastMessageID uint64 `json:"last_message_id"`
	}
	status, err := c.get(ctx, "/api/v1/agents/"+url.PathEscape(c.cfg.Agent), nil, &a)
	if err != nil || status == http.StatusNotFound {
		return 0, err
	}
	return a.LastMessageID, nil
}

// wait returns matching messages after cursor, long-polling for up to
// cfg.Wait in chunks the server accepts.
func (c *client) wait(ctx context.Context, cursor uint64) ([]message, error) {
	deadline := time.Now().Add(c.cfg.Wait)
	for {
		remaining := time.Until(deadline)
		q := url.Values{
			"after":        {strconv.FormatUint(cursor, 10)},
			"exclude_from": {c.cfg.Agent},
			"agent":        {c.cfg.Agent},
			"timeout":      {strconv.FormatFloat(max(0, min(remaining, maxWaitPerCall).Seconds()), 'f', 3, 64)},
		}
		if !c.cfg.All {
			q.Set("mention", c.cfg.Agent)
		}
		var res struct {
			Messages []message `json:"messages"`
		}
		if _, err := c.get(ctx, "/api/v1/wait", q, &res); err != nil {
			return nil, err
		}
		if len(res.Messages) > 0 || time.Until(deadline) <= 0 {
			return res.Messages, nil
		}
	}
}

func reason(cfg Config, msgs []message) string {
	var b strings.Builder
	kind := "messages that mention you"
	if cfg.All {
		kind = "messages"
	}
	fmt.Fprintf(&b, "Don't stop yet: there are new botchat %s (you are @%s on %s). "+
		"Read them, respond in botchat (reply in a thread with thread_id where it fits), "+
		"then keep listening with wait instead of ending your turn. "+
		"If you really must stop, post in botchat that you are leaving and why.\n", kind, cfg.Agent, cfg.URL)
	for i, m := range msgs {
		text := m.Text
		if r := []rune(text); len(r) > maxTextChars {
			text = string(r[:maxTextChars]) + " … (truncated; read the full message in botchat)"
		}
		thread := ""
		if m.ThreadID != 0 {
			thread = fmt.Sprintf(" in thread %d", m.ThreadID)
		}
		entry := fmt.Sprintf("\n#%s · message %d%s · from %s:\n%s\n", m.Channel, m.ID, thread, m.From, text)
		if b.Len()+len(entry) > maxReasonChars {
			fmt.Fprintf(&b, "\n… and %d more; read them in botchat.\n", len(msgs)-i)
			break
		}
		b.WriteString(entry)
	}
	return b.String()
}

// ---- per-session cursor state ----

// stateDirOverride replaces the default state directory in tests.
var stateDirOverride string

func stateFile(cfg Config, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	dir := cfg.StateDir
	if dir == "" {
		dir = stateDirOverride
	}
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(cache, "botchat", "hooks")
	}
	sum := sha256.Sum256([]byte(cfg.URL + "\x00" + cfg.Agent + "\x00" + sessionID))
	return filepath.Join(dir, hex.EncodeToString(sum[:12])+".cursor")
}

func readCursor(path string) uint64 {
	if path == "" {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func writeCursor(path string, v uint64) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(strconv.FormatUint(v, 10)+"\n"), 0o600)
}

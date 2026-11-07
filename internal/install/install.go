// Package install implements `botchat install`, which finds coding agents on
// the machine and registers botchat as an MCP server in the ones the user
// picks. See SPEC.md INS-*.
package install

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
)

// DefaultURL is the default botchat server URL (INS-3).
const DefaultURL = "http://127.0.0.1:7878"

// ServerName is the name of the MCP server entry botchat installs.
const ServerName = "botchat"

// Config is the parsed command line of `botchat install`.
type Config struct {
	URL    string
	Token  string
	Agents string
	Force  bool
	DryRun bool
}

// MCPURL is the MCP endpoint the installed entries point at.
func (c Config) MCPURL() string { return c.URL + "/mcp" }

// Env is everything the installer touches outside the process, so tests can
// substitute a fake home directory and fake agent CLIs.
type Env struct {
	Home      string // user home directory
	ConfigDir string // os.UserConfigDir
	LookPath  func(file string) (string, error)
	// Run executes a program in dir and returns its combined output. A
	// non-nil error means it could not be started or exited non-zero.
	Run    func(ctx context.Context, dir, path string, args ...string) (string, error)
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// SystemEnv returns an Env for the real machine.
func SystemEnv(stdin io.Reader, stdout, stderr io.Writer) Env {
	home, _ := os.UserHomeDir()
	cfg, _ := os.UserConfigDir()
	return Env{
		Home:      home,
		ConfigDir: cfg,
		LookPath:  exec.LookPath,
		Run: func(ctx context.Context, dir, path string, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, path, args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	}
}

// Main runs `botchat install <args>` on the real machine and returns the exit code.
func Main(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	return MainEnv(ctx, args, getenv, SystemEnv(stdin, stdout, stderr))
}

// MainEnv is Main with an explicit environment.
func MainEnv(ctx context.Context, args []string, getenv func(string) string, env Env) int {
	cfg, err := parse(args, getenv, env.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "botchat install:", err)
		return 2
	}
	return run(ctx, cfg, env)
}

func parse(args []string, getenv func(string) string, stderr io.Writer) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("botchat install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	def := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	fs.StringVar(&c.URL, "url", def("BOTCHAT_URL", DefaultURL), "botchat server URL (env BOTCHAT_URL)")
	fs.StringVar(&c.Token, "token", getenv("BOTCHAT_TOKEN"), "server token, if it requires one (env BOTCHAT_TOKEN)")
	fs.StringVar(&c.Agents, "agents", "", "comma-separated agent IDs to install into, or \"all\"; skips the prompt")
	fs.BoolVar(&c.Force, "force", false, "replace an existing botchat entry")
	fs.BoolVar(&c.DryRun, "dry-run", false, "show what would change without changing anything")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: botchat install [flags]

Finds coding agents on this machine (Claude Code, Codex, Cursor, Gemini CLI,
opencode, VS Code, Windsurf) and adds botchat to the ones you choose as a
user-level MCP server.

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
	c.URL = strings.TrimRight(c.URL, "/")
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("--url must be an http(s) URL, got %q", c.URL)
	}
	return c, nil
}

type detected struct {
	agent     *agent
	installed bool
	err       error // reading the existing configuration failed
}

func run(ctx context.Context, cfg Config, env Env) int {
	var found []detected
	for _, a := range agents {
		if !a.detect(env) {
			continue
		}
		ok, err := a.has(ctx, env)
		found = append(found, detected{agent: a, installed: ok, err: err})
	}
	out := env.Stdout
	if len(found) == 0 {
		fmt.Fprintln(out, "No supported coding agents found.")
		fmt.Fprintf(out, "Supported: %s.\n", supportedList())
		return 0
	}

	fmt.Fprintf(out, "Found %d coding agent%s:\n\n", len(found), plural(len(found)))
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for i, d := range found {
		note := ""
		switch {
		case d.err != nil:
			note = "can't read config: " + d.err.Error()
		case d.installed:
			note = "already installed"
		}
		fmt.Fprintf(tw, "  %d. %s\t%s\t%s\n", i+1, d.agent.Name, d.agent.ID, note)
	}
	tw.Flush()
	fmt.Fprintf(out, "\nMCP server: %s\n\n", cfg.MCPURL())

	selection := cfg.Agents
	if selection == "" {
		fmt.Fprint(out, "Install botchat for which agents? [all, numbers or IDs; Enter for none]: ")
		line, err := readLine(env.Stdin)
		fmt.Fprintln(out)
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintln(env.Stderr, "botchat install:", err)
			return 1
		}
		selection = line
	}
	chosen, err := choose(selection, found)
	if err != nil {
		fmt.Fprintln(env.Stderr, "botchat install:", err)
		return 2
	}
	if len(chosen) == 0 {
		fmt.Fprintln(out, "Nothing installed.")
		return 0
	}

	failed := false
	changed := false
	for _, d := range chosen {
		a := d.agent
		if d.installed && !cfg.Force {
			fmt.Fprintf(out, "%s: already installed (use --force to replace)\n", a.Name)
			continue
		}
		if cfg.DryRun {
			fmt.Fprintf(out, "%s: would %s\n", a.Name, indent(redact(a.plan(env, cfg, d.installed), cfg.Token)))
			continue
		}
		msg, err := a.install(ctx, env, cfg, d.installed)
		if err != nil {
			failed = true
			fmt.Fprintf(out, "%s: failed: %s\n", a.Name, indent(redact(err.Error(), cfg.Token)))
			continue
		}
		changed = true
		fmt.Fprintf(out, "%s: %s\n", a.Name, msg)
	}
	if changed {
		fmt.Fprintln(out, "\nRestart running agents to pick up the change. Each session picks its name with the join tool.")
		if cfg.Token != "" && hasAgent(chosen, "codex") {
			fmt.Fprintln(out, "Codex reads the token from BOTCHAT_TOKEN, so set it in the environment Codex runs in.")
		}
	}
	if failed {
		return 1
	}
	return 0
}

func readLine(r io.Reader) (string, error) {
	// Read byte by byte so nothing past the line is consumed.
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return b.String(), err
		}
	}
}

// choose resolves a selection ("all", numbers, IDs) against detected agents (INS-2).
func choose(sel string, found []detected) ([]detected, error) {
	fields := strings.FieldsFunc(sel, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' })
	var out []detected
	seen := map[string]bool{}
	add := func(d detected) {
		if !seen[d.agent.ID] {
			seen[d.agent.ID] = true
			out = append(out, d)
		}
	}
	for _, f := range fields {
		f = strings.ToLower(f)
		if f == "all" {
			for _, d := range found {
				add(d)
			}
			continue
		}
		if n, err := strconv.Atoi(f); err == nil {
			if n < 1 || n > len(found) {
				return nil, fmt.Errorf("no agent numbered %d", n)
			}
			add(found[n-1])
			continue
		}
		i := indexOf(found, f)
		if i < 0 {
			if lookup(f) != nil {
				return nil, fmt.Errorf("%s was not found on this machine", f)
			}
			return nil, fmt.Errorf("unknown agent %q (supported: %s)", f, supportedIDs())
		}
		add(found[i])
	}
	return out, nil
}

func indexOf(found []detected, id string) int {
	for i, d := range found {
		if d.agent.ID == id {
			return i
		}
	}
	return -1
}

func hasAgent(ds []detected, id string) bool { return indexOf(ds, id) >= 0 }

func lookup(id string) *agent {
	for _, a := range agents {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func supportedList() string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

func supportedIDs() string {
	ids := make([]string, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
	}
	return strings.Join(ids, ", ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<token>")
}

func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func join(base string, elem ...string) string {
	return filepath.Join(append([]string{base}, elem...)...)
}

package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// agent is a coding agent botchat knows how to configure, either through the
// agent's own CLI or by editing its JSON config file (see SPEC.md Installer).
type agent struct {
	ID   string
	Name string
	Bins []string           // detected if any is on PATH
	Dirs func(Env) []string // ... or if any of these exists
	cli  *cliSpec           // configured with the agent's CLI
	file *fileSpec          // configured by editing a JSON file
}

type cliSpec struct {
	bin    string
	get    []string
	remove []string
	add    func(Config) []string
}

type fileSpec struct {
	path      func(Env) string
	container string // top-level key holding the servers
	entry     func(Config) any
}

func bearer(c Config) map[string]string {
	if c.Token == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + c.Token}
}

var agents = []*agent{
	{
		ID: "claude", Name: "Claude Code", Bins: []string{"claude"},
		cli: &cliSpec{
			bin:    "claude",
			get:    []string{"mcp", "get", ServerName},
			remove: []string{"mcp", "remove", "--scope", "user", ServerName},
			add: func(c Config) []string {
				args := []string{"mcp", "add", "--scope", "user", "--transport", "http", ServerName, c.MCPURL()}
				if c.Token != "" {
					args = append(args, "--header", "Authorization: Bearer "+c.Token)
				}
				return args
			},
		},
	},
	{
		ID: "codex", Name: "Codex", Bins: []string{"codex"},
		cli: &cliSpec{
			bin:    "codex",
			get:    []string{"mcp", "get", ServerName},
			remove: []string{"mcp", "remove", ServerName},
			add: func(c Config) []string {
				args := []string{"mcp", "add", ServerName, "--url", c.MCPURL()}
				if c.Token != "" {
					args = append(args, "--bearer-token-env-var", "BOTCHAT_TOKEN")
				}
				return args
			},
		},
	},
	{
		ID: "cursor", Name: "Cursor", Bins: []string{"cursor"},
		Dirs: func(e Env) []string { return []string{join(e.Home, ".cursor")} },
		file: &fileSpec{
			path:      func(e Env) string { return join(e.Home, ".cursor", "mcp.json") },
			container: "mcpServers",
			entry: func(c Config) any {
				return struct {
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers,omitempty"`
				}{c.MCPURL(), bearer(c)}
			},
		},
	},
	{
		// ~/.gemini is also used by other Google tools, so only the binary counts.
		ID: "gemini", Name: "Gemini CLI", Bins: []string{"gemini"},
		file: &fileSpec{
			path:      func(e Env) string { return join(e.Home, ".gemini", "settings.json") },
			container: "mcpServers",
			entry: func(c Config) any {
				return struct {
					HTTPURL string            `json:"httpUrl"`
					Headers map[string]string `json:"headers,omitempty"`
				}{c.MCPURL(), bearer(c)}
			},
		},
	},
	{
		ID: "opencode", Name: "opencode", Bins: []string{"opencode"},
		Dirs: func(e Env) []string { return []string{join(e.Home, ".config", "opencode")} },
		file: &fileSpec{
			path:      func(e Env) string { return join(e.Home, ".config", "opencode", "opencode.json") },
			container: "mcp",
			entry: func(c Config) any {
				return struct {
					Type    string            `json:"type"`
					URL     string            `json:"url"`
					Enabled bool              `json:"enabled"`
					Headers map[string]string `json:"headers,omitempty"`
				}{"remote", c.MCPURL(), true, bearer(c)}
			},
		},
	},
	{
		ID: "vscode", Name: "VS Code", Bins: []string{"code"},
		Dirs: func(e Env) []string { return []string{join(e.ConfigDir, "Code", "User")} },
		file: &fileSpec{
			path:      func(e Env) string { return join(e.ConfigDir, "Code", "User", "mcp.json") },
			container: "servers",
			entry: func(c Config) any {
				return struct {
					Type    string            `json:"type"`
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers,omitempty"`
				}{"http", c.MCPURL(), bearer(c)}
			},
		},
	},
	{
		ID: "windsurf", Name: "Windsurf", Bins: []string{"windsurf"},
		Dirs: func(e Env) []string { return []string{join(e.Home, ".codeium", "windsurf")} },
		file: &fileSpec{
			path:      func(e Env) string { return join(e.Home, ".codeium", "windsurf", "mcp_config.json") },
			container: "mcpServers",
			entry: func(c Config) any {
				return struct {
					ServerURL string            `json:"serverUrl"`
					Headers   map[string]string `json:"headers,omitempty"`
				}{c.MCPURL(), bearer(c)}
			},
		},
	},
}

func (a *agent) detect(env Env) bool {
	for _, b := range a.Bins {
		if _, err := env.LookPath(b); err == nil {
			return true
		}
	}
	if a.Dirs != nil && env.Home != "" {
		for _, d := range a.Dirs(env) {
			if exists(d) {
				return true
			}
		}
	}
	return false
}

// has reports whether the agent already has a botchat server (INS-1).
func (a *agent) has(ctx context.Context, env Env) (bool, error) {
	if a.cli != nil {
		path, err := env.LookPath(a.cli.bin)
		if err != nil {
			return false, nil
		}
		_, err = env.Run(ctx, path, a.cli.get...)
		return err == nil, nil
	}
	obj, err := readObject(a.file.path(env))
	if err != nil || obj == nil {
		return false, err
	}
	servers, err := servers(obj, a.file.container)
	if err != nil {
		return false, err
	}
	_, ok := servers.get(ServerName)
	return ok, nil
}

// plan describes what install would do (INS-6).
func (a *agent) plan(env Env, cfg Config, replace bool) string {
	if a.cli != nil {
		var b strings.Builder
		b.WriteString("run:")
		if replace {
			b.WriteString("\n" + cmdline(a.cli.bin, a.cli.remove))
		}
		b.WriteString("\n" + cmdline(a.cli.bin, a.cli.add(cfg)))
		return b.String()
	}
	return fmt.Sprintf("write %s:\n%s", a.file.path(env), a.snippet(cfg))
}

func (a *agent) install(ctx context.Context, env Env, cfg Config, replace bool) (string, error) {
	if a.cli != nil {
		path, err := env.LookPath(a.cli.bin)
		if err != nil {
			return "", err
		}
		if replace {
			// The entry may live in another scope; adding still works, so
			// a failed remove is not fatal.
			_, _ = env.Run(ctx, path, a.cli.remove...)
		}
		args := a.cli.add(cfg)
		if out, err := env.Run(ctx, path, args...); err != nil {
			return "", fmt.Errorf("%s: %v\n%s", cmdline(a.cli.bin, args), err, strings.TrimSpace(out))
		}
		verb := "added"
		if replace {
			verb = "replaced"
		}
		return fmt.Sprintf("%s (%s)", verb, cmdline(a.cli.bin, a.cli.add(Config{URL: cfg.URL}))), nil
	}
	return a.installFile(env, cfg)
}

func (a *agent) installFile(env Env, cfg Config) (string, error) {
	path := a.file.path(env)
	manual := func(err error) error {
		return fmt.Errorf("%s: %v\nAdd this by hand:\n%s", path, err, a.snippet(cfg))
	}
	obj, err := readObject(path)
	if err != nil {
		return "", manual(err)
	}
	srv, err := servers(obj, a.file.container)
	if err != nil {
		return "", manual(err)
	}
	_, replaced := srv.get(ServerName)
	entry, err := marshal(a.file.entry(cfg))
	if err != nil {
		return "", err
	}
	srv = srv.set(ServerName, entry)
	srvJSON, err := srv.MarshalJSON()
	if err != nil {
		return "", err
	}
	obj = obj.set(a.file.container, srvJSON)
	raw, err := obj.MarshalJSON()
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return "", err
	}
	out.WriteByte('\n')
	if err := writeFileAtomic(path, out.Bytes()); err != nil {
		return "", err
	}
	if replaced {
		return "replaced in " + path, nil
	}
	return "added to " + path, nil
}

// snippet is the JSON a user would merge into the config by hand.
func (a *agent) snippet(cfg Config) string {
	entry, _ := marshal(a.file.entry(cfg))
	raw, _ := object{{a.file.container, mustJSON(object{{ServerName, entry}})}}.MarshalJSON()
	var out bytes.Buffer
	_ = json.Indent(&out, raw, "", "  ")
	return out.String()
}

// readObject reads a JSON object file, keeping key order. A missing file is
// an empty object; nil means the file does not exist.
func readObject(path string) (object, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseObject(b)
}

func servers(obj object, container string) (object, error) {
	raw, ok := obj.get(container)
	if !ok {
		return object{}, nil
	}
	s, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", container, err)
	}
	return s, nil
}

func marshal(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

func mustJSON(o object) json.RawMessage {
	b, _ := o.MarshalJSON()
	return b
}

// writeFileAtomic replaces path with data via a temp file and rename,
// keeping the existing file's mode and following a symlink to its target
// (INS-4).
func writeFileAtomic(path string, data []byte) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(dir, ".botchat-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func cmdline(bin string, args []string) string {
	parts := []string{bin}
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$&|;<>()*?!`\\") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

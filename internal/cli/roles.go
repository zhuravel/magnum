package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
)

const rolesUsage = "[--repo owner/name] [--kinds] [--json]"

type rolesOpts struct {
	repo        string
	kinds, json bool
}

func newRolesCmd(c *Context) *cobra.Command {
	var o rolesOpts
	cmd := newCommand(groupInspect, "roles "+rolesUsage, "the effective review roles per watch (--kinds: the agent CLIs magnum drives)",
		"Print the review roles each [[watch]] runs, resolved from [[role]] and the built-in defaults: name, kind, "+
			"runs, model, effort, capture, report file, initial prompt (and whether prompts_dir or the embedded copy "+
			"provides it), the roles it runs after, the judge and the aliases --role accepts. --repo shows the roles "+
			"of the watch covering one repository. --kinds prints the [kinds.<name>] specs instead: wrapper mode, "+
			"login check, the resume, model, effort, subagents, MCP-off and name args, the session source and the command line each "+
			"role starts with. --json prints the resolved roles (or kinds) as JSON keyed like config.toml.",
		func(pos []string) int { return runRoles(c, o, pos) })
	fs := cmd.Flags()
	fs.StringVar(&o.repo, "repo", "", "only the watch covering this repository (owner/name or name)")
	fs.BoolVar(&o.kinds, "kinds", false, "print the agent kinds ([kinds.<name>]) instead of the roles")
	fs.BoolVar(&o.json, "json", false, "print JSON")
	_ = cmd.RegisterFlagCompletionFunc("repo", completeFlag(c.completeRepos))
	return cmd
}

// rolesGroup is the roles of one watch (or of a repository no watch covers).
type rolesGroup struct {
	Watch *config.Watch
	Repo  string // --repo, when given
	Roles []config.Role
}

func runRoles(c *Context, o rolesOpts, pos []string) int {
	if len(pos) > 0 {
		return inspUsage(c, "roles", "unexpected argument "+pos[0], rolesUsage)
	}
	if err := c.LoadConfig(); err != nil {
		fmt.Fprintln(c.Stderr, err)
		return 1
	}
	cfg := c.Config
	if o.kinds {
		return rolesKinds(c, cfg, o)
	}
	var groups []rolesGroup
	switch {
	case o.repo != "":
		full, err := rolesRepo(cfg, o.repo)
		if err != nil {
			return inspUsage(c, "roles", err.Error(), rolesUsage)
		}
		w := cfg.WatchFor(full)
		groups = append(groups, rolesGroup{Watch: w, Repo: full, Roles: cfg.RolesFor(w)})
	case len(cfg.Watches) == 0:
		groups = append(groups, rolesGroup{Roles: cfg.RolesFor(nil)})
	default:
		for i := range cfg.Watches {
			w := &cfg.Watches[i]
			groups = append(groups, rolesGroup{Watch: w, Roles: cfg.RolesFor(w)})
		}
	}
	if o.json {
		return rolesJSON(c, cfg, groups)
	}
	rolesRender(c.Stdout, cfg, groups)
	return 0
}

// rolesRepo normalizes --repo: owner/name, or a name in daemon.default_repo's
// owner.
func rolesRepo(cfg *config.Config, s string) (string, error) { return repoArg(cfg, "--repo ", s) }

// repoArg normalizes a repository argument: owner/name, or a name in
// daemon.default_repo's owner. An error names it after label ("--repo ",
// or "" for a positional one).
func repoArg(cfg *config.Config, label, s string) (string, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok {
		def, _, _ := strings.Cut(cfg.Daemon.DefaultRepo, "/")
		if def == "" {
			return "", fmt.Errorf("%s%s: want owner/name (no daemon.default_repo to take the owner from)", label, s)
		}
		owner, name = def, s
	}
	if owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%s%s: want owner/name or name", label, s)
	}
	return owner + "/" + name, nil
}

// rolesIsDir reports whether path is a directory.
func rolesIsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// rolesHeader is the line above a watch's roles.
func rolesHeader(g rolesGroup) string {
	w := g.Watch
	if w == nil {
		if g.Repo != "" {
			return g.Repo + ": no [[watch]] covers it; a PR added with `magnum review` runs every role"
		}
		return "every role (no [[watch]] configured)"
	}
	repos := "*"
	if len(w.Include) > 0 {
		repos = strings.Join(w.Include, ",")
	}
	h := fmt.Sprintf("watch %s/%s", w.Owner, repos)
	if len(w.Exclude) > 0 {
		h += " (excluding " + strings.Join(w.Exclude, ",") + ")"
	}
	if g.Repo != "" {
		h = g.Repo + ": " + h
	}
	if w.Identity != "" {
		h += ", posts as " + w.Identity
	}
	if len(w.Roles) == 0 {
		return h + ", every role"
	}
	return h + ", roles " + strings.Join(w.Roles, ", ")
}

// rolesPrompt describes a role's initial prompt: the file and its source;
// "-" for a shell role driven by its command.
func rolesPrompt(cfg *config.Config, r config.Role) (name, source string) {
	name = r.PromptFile(config.PromptInitial)
	if name == "" {
		return "-", ""
	}
	p, err := cfg.RolePrompt(r, config.PromptInitial)
	switch {
	case err != nil:
		return name, "missing"
	case p.Embedded:
		return name, "embedded"
	}
	return name, "prompts_dir"
}

func rolesRender(w io.Writer, cfg *config.Config, groups []rolesGroup) {
	dir := cfg.Pipeline.PromptsDir
	switch {
	case dir == "":
		fmt.Fprintln(w, "prompts_dir: none (embedded prompts only)")
	case !rolesIsDir(dir):
		fmt.Fprintf(w, "prompts_dir: %s (missing: embedded prompts only)\n", inspTilde(dir))
	default:
		fmt.Fprintf(w, "prompts_dir: %s\n", inspTilde(dir))
	}
	for _, g := range groups {
		fmt.Fprintf(w, "\n%s\n", rolesHeader(g))
		if len(g.Roles) == 0 {
			fmt.Fprintln(w, "  no roles")
			continue
		}
		tw := inspTable(w)
		fmt.Fprintln(tw, "  NAME\tKIND\tRUNS\tMODEL\tEFFORT\tCAPTURE\tOUTPUT\tPROMPT\tAFTER\tJUDGE\tALIASES")
		var commands []string
		for _, r := range g.Roles {
			kind := r.Kind
			if r.IsShell() && r.Tool != "" {
				kind += ":" + r.Tool
			}
			prompt, src := rolesPrompt(cfg, r)
			if src != "" {
				prompt += " (" + src + ")"
			}
			judge := ""
			if r.Judge {
				judge = "yes"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, kind, r.Runs, inspOrDash(r.Model), inspOrDash(r.Effort),
				r.Capture, r.ReportFile(), prompt, inspOrDash(strings.Join(r.After, ",")), inspOrDash(judge), inspOrDash(strings.Join(r.Aliases, ",")))
			if r.IsShell() && r.Command != "" {
				commands = append(commands, fmt.Sprintf("  %s runs: %s", r.Name, strings.Join(append([]string{r.Command}, r.Args...), " ")))
			}
		}
		tw.Flush()
		for _, line := range commands {
			fmt.Fprintln(w, line)
		}
	}
}

func rolesJSON(c *Context, cfg *config.Config, groups []rolesGroup) int {
	type group struct {
		Watch   string            `json:"watch,omitempty"`
		Include []string          `json:"include,omitempty"`
		Exclude []string          `json:"exclude,omitempty"`
		Repo    string            `json:"repo,omitempty"`
		Roles   []json.RawMessage `json:"roles"`
	}
	out := make([]group, 0, len(groups))
	for _, g := range groups {
		gj := group{Repo: g.Repo, Roles: []json.RawMessage{}}
		if g.Watch != nil {
			gj.Watch, gj.Include, gj.Exclude = g.Watch.Owner, g.Watch.Include, g.Watch.Exclude
		}
		for _, r := range g.Roles {
			_, src := rolesPrompt(cfg, r)
			gj.Roles = append(gj.Roles, tomlJSON(r, "prompt_source", src))
		}
		out = append(out, gj)
	}
	if err := writeJSON(c.Stdout, out); err != nil {
		fmt.Fprintln(c.Stderr, err)
		return 1
	}
	return 0
}

// rolesKindNames lists the agent kinds of the configured roles
// (config.Role.AgentKind; a shell role's tool counts), in [[role]] order.
func rolesKindNames(cfg *config.Config) []string {
	var out []string
	for _, r := range cfg.RolesFor(nil) {
		if k := r.AgentKind(); k != "" && k != config.KindShell && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// rolesAllKinds lists the kinds of the configured roles first, then every
// other declared kind (sorted). Without a config: the built-in kinds.
func rolesAllKinds(cfg *config.Config) []string {
	if cfg == nil {
		cfg = config.Defaults()
	}
	out := rolesKindNames(cfg)
	for _, k := range cfg.KindNames() {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// rolesJudgeName is the judge role every watch shares, for help lines; ""
// when there is no config or the watches' judges differ.
func rolesJudgeName(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if len(cfg.Watches) == 0 {
		return cfg.JudgeFor(nil).Name
	}
	name := ""
	for i := range cfg.Watches {
		j := cfg.JudgeFor(&cfg.Watches[i]).Name
		if name != "" && j != name {
			return ""
		}
		name = j
	}
	return name
}

// rolesLoginCommand is the command that logs an agent kind's CLI in.
func rolesLoginCommand(kind string) string {
	switch kind {
	case config.KindCodex:
		return "codex login"
	case config.KindClaude:
		return "claude auth login"
	}
	return kind + " login"
}

// rolesUsers lists the roles that run an agent kind.
func rolesUsers(cfg *config.Config, kind string) []string {
	var out []string
	for _, r := range cfg.RolesFor(nil) {
		if r.AgentKind() == kind {
			out = append(out, r.Name)
		}
	}
	return out
}

func rolesKinds(c *Context, cfg *config.Config, o rolesOpts) int {
	if o.repo != "" {
		return inspUsage(c, "roles", "--repo selects roles; it does not apply to --kinds", rolesUsage)
	}
	kinds := rolesAllKinds(cfg)
	if o.json {
		type kindJSON struct {
			Name   string          `json:"name"`
			UsedBy []string        `json:"used_by"`
			Spec   json.RawMessage `json:"spec"`
		}
		out := make([]kindJSON, 0, len(kinds))
		for _, name := range kinds {
			k, _ := cfg.KindSpec(name)
			out = append(out, kindJSON{Name: name, UsedBy: append([]string{}, rolesUsers(cfg, name)...), Spec: tomlJSON(k)})
		}
		if err := writeJSON(c.Stdout, out); err != nil {
			fmt.Fprintln(c.Stderr, err)
			return 1
		}
		return 0
	}
	w := c.Stdout
	args := func(group []string) string { return inspOrDash(strings.Join(group, " ")) }
	for i, name := range kinds {
		k, _ := cfg.KindSpec(name)
		users := rolesUsers(cfg, name)
		if i > 0 {
			fmt.Fprintln(w)
		}
		head := name
		if len(users) > 0 {
			head += " (used by " + strings.Join(users, ", ") + ")"
		} else {
			head += " (no role uses it)"
		}
		fmt.Fprintln(w, head)
		login := "none"
		if k.LoginCheck != "" {
			ok := k.LoginOK
			if ok == "" {
				ok = "exit status 0"
			}
			login = k.LoginCheck + " (logged in: " + ok + "; fix: " + rolesLoginCommand(name) + ")"
		}
		wrapper := k.Wrapper
		if wrapper == config.WrapperAuto {
			wrapper += " (zsh -ic 'whence -w " + name + "': a function or alias is a wrapper and gets no args)"
		}
		tw := inspTable(w)
		fmt.Fprintf(tw, "  wrapper:\t%s\n", wrapper)
		fmt.Fprintf(tw, "  login:\t%s\n", login)
		fmt.Fprintf(tw, "  resume:\t%s\n", args(k.Resume))
		fmt.Fprintf(tw, "  model:\t%s\n", args(k.Model))
		fmt.Fprintf(tw, "  effort:\t%s\n", args(k.Effort))
		fmt.Fprintf(tw, "  subagents:\t%s (none: %s)\n", args(k.Subagents), args(k.NoSubagents))
		var mcpOff []string
		if k.MCPOff && len(k.MCPStrict) > 0 {
			what := " (none of your MCP servers load)"
			if len(k.MCPDisable) > 0 {
				what = " (once, whatever the servers' names)" // e.g. Codex's apps connector, which no config table names
			}
			mcpOff = append(mcpOff, args(k.MCPStrict)+what)
		}
		if k.MCPOff && len(k.MCPDisable) > 0 {
			mcpOff = append(mcpOff, args(k.MCPDisable)+" per MCP server of the Codex config (allowed: "+inspOrDash(strings.Join(k.MCPAllow, ", "))+")")
		}
		mcp := "- (the MCP servers of the CLI's config load)"
		if len(mcpOff) > 0 {
			mcp = strings.Join(mcpOff, "; ")
		}
		fmt.Fprintf(tw, "  mcp:\t%s\n", mcp)
		project := "-"
		if paths, effect, servers := agents.ProjectRule(name); paths != "" {
			if len(k.ProjectUntrust) > 0 || servers && k.ProjectMCP == config.ProjectMCPOff && len(k.MCPDisable) > 0 {
				project = "- (a changed " + paths + " loads)"
				if len(k.ProjectUntrust) > 0 {
					project = args(k.ProjectUntrust) + " when the PR changes " + paths + " (" + effect + ")"
				}
				if servers {
					project += "; else its MCP servers: " + k.ProjectMCP
				}
			}
		}
		fmt.Fprintf(tw, "  project:\t%s\n", project)
		fmt.Fprintf(tw, "  name:\t%s\n", args(k.Name))
		fmt.Fprintf(tw, "  rename:\t%s\n", inspOrDash(k.Rename))
		fmt.Fprintf(tw, "  start:\t%s\n", args(k.Start))
		fmt.Fprintf(tw, "  args:\t%s\n", args(k.Args))
		fmt.Fprintf(tw, "  session:\t%s\n", k.SessionSource)
		fmt.Fprintf(tw, "  permission prompts:\t%s\n", inspOrDash(k.OnPermissionPrompt))
		fmt.Fprintf(tw, "  model switch:\t%s\n", rolesModelSwitch(k))
		for _, r := range cfg.RolesFor(nil) {
			if r.IsShell() || r.Kind != name {
				continue
			}
			argv := k.Argv(config.LaunchArgs{Title: config.PlaceholderTitle, Model: r.Model, Effort: r.Effort,
				Subagents: r.MaxSubagents, Wrapper: k.Wrapper == config.WrapperTrue, Extra: r.Args})
			fmt.Fprintf(tw, "  starts %s:\t%s\n", r.Name, strings.Join(append([]string{name}, argv...), " "))
		}
		tw.Flush()
	}
	return 0
}

// rolesModelSwitch is the kind's "model switch:" line: the command typed
// when a model hits its own limit, the fallbacks in order, the kind's
// default model and the argument that switches back to the CLI's default;
// "-" when the kind cannot switch (such a limit pauses the kind instead).
func rolesModelSwitch(k config.Kind) string {
	if k.SwitchModel == "" {
		return "- (a model limit pauses the kind)"
	}
	return fmt.Sprintf("%s, fallbacks %s, default_model %s, reset_model %s", k.SwitchModel,
		inspOrDash(strings.Join(k.FallbackModels, ", ")), inspOrDash(k.DefaultModel), inspOrDash(k.ResetModel))
}

// tomlJSON encodes a config struct as a JSON object keyed by its toml tags
// in field order (nested structs alike, a config.Duration as its string),
// followed by the extra key/value pairs.
func tomlJSON(v any, extra ...string) json.RawMessage {
	var enc func(rv reflect.Value, extra []string) []byte
	enc = func(rv reflect.Value, extra []string) []byte {
		if d, ok := rv.Interface().(config.Duration); ok {
			b, _ := json.Marshal(d.String())
			return b
		}
		if rv.Kind() != reflect.Struct {
			b, _ := json.Marshal(rv.Interface())
			return b
		}
		var b bytes.Buffer
		b.WriteByte('{')
		n := 0
		key := func(k string) {
			if n > 0 {
				b.WriteByte(',')
			}
			n++
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
		}
		t := rv.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
			if !f.IsExported() || name == "" || name == "-" {
				continue
			}
			key(name)
			b.Write(enc(rv.Field(i), nil))
		}
		for i := 0; i+1 < len(extra); i += 2 {
			key(extra[i])
			vb, _ := json.Marshal(extra[i+1])
			b.Write(vb)
		}
		b.WriteByte('}')
		return b.Bytes()
	}
	return enc(reflect.ValueOf(v), extra)
}

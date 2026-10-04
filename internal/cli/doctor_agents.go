package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// doctorInstall is the fix for an agent CLI that is not on PATH.
func doctorInstall(kind string) string {
	switch kind {
	case config.KindCodex:
		return "install Codex (`npm i -g @openai/codex` or brew) and put it on PATH"
	case config.KindClaude:
		return "install Claude Code (`npm i -g @anthropic-ai/claude-code`) and put it on PATH"
	}
	return "install the " + kind + " CLI and put it on PATH (or fix the role's kind in config.toml)"
}

// doctorWhence asks the user's zsh what kind names, as herdr's panes see it:
// "function", "alias", "command", "builtin", ... or "none".
func doctorWhence(ctx context.Context, d doctorDeps, kind string) (string, error) {
	out, err := doctorExec(ctx, d, 15*time.Second, "", nil, "zsh", "-ic", "whence -w "+kind)
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), kind+":"); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("zsh printed no `whence -w %s` line", kind)
}

// doctorAutonomy are the flags that let a built-in agent CLI work without
// approval prompts (the first is the [kinds.*] args default). A plain
// binary started without one of them stops at prompts magnum answers No,
// and a sandboxed Codex judge cannot reach the network to post. Each entry
// is one flag, or a flag and its value as two args.
var doctorAutonomy = map[string][][]string{
	config.KindCodex:  {{"--dangerously-bypass-approvals-and-sandbox"}, {"--yolo"}},
	config.KindClaude: {{"--dangerously-skip-permissions"}, {"--permission-mode", "bypassPermissions"}, {"--permission-mode=bypassPermissions"}},
}

// doctorAutonomous reports whether argv carries one of flags.
func doctorAutonomous(argv []string, flags [][]string) bool {
	for _, f := range flags {
		for i := range argv {
			if i+len(f) <= len(argv) && slices.Equal(argv[i:i+len(f)], f) {
				return true
			}
		}
	}
	return false
}

// doctorPlainBinary judges a kind whose CLI is a plain binary (no wrapper),
// so magnum launches it with the kind's start and args plus each role's
// args. A built-in kind whose session roles miss its autonomy flag fails;
// another kind without args only warns (magnum knows no flag for it).
func doctorPlainBinary(d doctorDeps, kind string, k config.Kind) doctorCheck {
	name, mode, section := kind+" wrapper", k.Wrapper, "[kinds."+kind+"]"
	flags, known := doctorAutonomy[kind]
	if !known {
		if len(k.Args) == 0 {
			return doctorWarned(name, fmt.Sprintf("%s is a plain binary (wrapper %s) and %s args is empty: agents start without your usual flags and may stop at approval prompts", kind, mode, section),
				"put the flags into "+section+" args in config.toml, or restore the zsh `"+kind+"` function")
		}
		return doctorOK(name, fmt.Sprintf("%s is a plain binary (wrapper %s): magnum adds %s args", kind, mode, section))
	}
	var missing []string
	sessions := 0
	for _, r := range d.Config.RolesFor(nil) {
		if !r.IsAgent() || r.Kind != kind {
			continue
		}
		sessions++
		if !doctorAutonomous(k.Argv(config.LaunchArgs{Extra: r.Args}), flags) {
			missing = append(missing, r.Name)
		}
	}
	flag := flags[0][0]
	switch {
	case sessions == 0:
		return doctorOK(name, fmt.Sprintf("%s is a plain binary (wrapper %s); no session role starts it", kind, mode))
	case len(missing) > 0:
		return doctorFailed(name, fmt.Sprintf("%s is a plain binary (wrapper %s) and starts %s without %s: the agents stop at approval prompts, which magnum answers No, and a sandboxed judge cannot post",
			kind, mode, strings.Join(missing, ", "), flag),
			fmt.Sprintf("set %s args = [%q] in ~/.config/magnum/config.toml (the default), or restore the zsh `%s` function that adds it", section, flag, kind))
	}
	return doctorOK(name, fmt.Sprintf("%s is a plain binary (wrapper %s): magnum adds %s args, which run it without approval prompts", kind, mode, section))
}

// doctorAgentChecks checks every agent kind the configured roles use (a
// shell role's tool counts): the CLI is on PATH in zsh, its login check
// passes ([kinds.<name>] login_check and login_ok), and how magnum passes
// its args (wrapper detection; a plain binary must get the kind's autonomy
// flag, see doctorPlainBinary).
func doctorAgentChecks(ctx context.Context, d doctorDeps) []doctorCheck {
	var out, wrappers []doctorCheck
	for _, kind := range rolesKindNames(d.Config) {
		k, _ := d.Config.KindSpec(kind)
		users := strings.Join(rolesUsers(d.Config, kind), ", ")
		what, werr := doctorWhence(ctx, d, kind)
		onPath := werr == nil && what != "none"
		switch {
		case werr != nil:
			out = append(out, doctorFailed(kind, fmt.Sprintf("could not ask zsh for %s (used by %s): %s", kind, users, werr.Error()), doctorInstall(kind)))
		case !onPath:
			out = append(out, doctorFailed(kind, fmt.Sprintf("%s (used by %s) is not on PATH in zsh", kind, users), doctorInstall(kind)))
		default:
			label := kind
			if version, err := doctorExec(ctx, d, 15*time.Second, "", nil, kind, "--version"); err == nil && version != "" {
				label = kind + " " + inspFirstLine(version)
			}
			out = append(out, doctorOK(kind, fmt.Sprintf("%s is on PATH (zsh %s; used by %s)", label, what, users)))
		}
		out = append(out, doctorLogin(ctx, d, kind, k, onPath))

		mode := k.Wrapper
		section := "[kinds." + kind + "]"
		isWrapper, err := d.Agents.Wrapper(ctx, kind)
		name := kind + " wrapper"
		switch {
		case err != nil:
			wrappers = append(wrappers, doctorWarned(name, kind+" wrapper detection failed: "+err.Error(),
				"set "+section+" wrapper = \"true\" or \"false\" in config.toml"))
		case isWrapper:
			wrappers = append(wrappers, doctorOK(name, fmt.Sprintf("%s is a zsh wrapper function (wrapper %s): magnum passes only resume, name, model and effort args", kind, mode)))
		default:
			wrappers = append(wrappers, doctorPlainBinary(d, kind, k))
		}
	}
	return append(out, wrappers...)
}

// doctorLogin runs a kind's login check (config.Kind.LoginArgv, read by
// config.Kind.LoggedIn).
func doctorLogin(ctx context.Context, d doctorDeps, kind string, k config.Kind, onPath bool) doctorCheck {
	name := kind + " login"
	argv := k.LoginArgv()
	check := strings.Join(argv, " ")
	switch {
	case len(argv) == 0:
		return doctorSkipped(name, fmt.Sprintf("%s has no login check ([kinds.%s] login_check)", kind, kind))
	case !onPath && argv[0] == kind:
		return doctorSkipped(name, fmt.Sprintf("`%s` not run: %s is not on PATH", check, kind))
	case d.Run == nil:
		return doctorSkipped(name, fmt.Sprintf("`%s` not run: no runner", check))
	}
	res, err := d.Run.Run(ctx, execx.Cmd{Name: argv[0], Args: argv[1:], Timeout: 15 * time.Second, Label: "doctor"})
	var ee *execx.ExitError
	if err != nil && !errors.As(err, &ee) {
		return doctorFailed(name, fmt.Sprintf("cannot run `%s`: %s", check, err.Error()), doctorInstall(argv[0]))
	}
	stdout, stderr := string(res.Stdout), string(res.Stderr)
	if ee != nil && stderr == "" {
		stderr = ee.Stderr
	}
	loggedIn, readable := k.LoggedIn(stdout, stderr, err == nil)
	switch {
	case !readable:
		return doctorWarned(name, fmt.Sprintf("cannot read the output of `%s` with login_ok %q: %s", check, k.LoginOK, inspFirstLine(execx.Redact(stdout+"\n"+stderr))),
			"fix [kinds."+kind+"] login_check or login_ok in config.toml (magnum assumes logged in meanwhile)")
	case !loggedIn:
		return doctorFailed(name, fmt.Sprintf("%s is logged out (`%s`)", kind, check), "run `"+rolesLoginCommand(kind)+"` (reviews pause until it passes)")
	}
	return doctorOK(name, fmt.Sprintf("%s is logged in (`%s`)", kind, check))
}

// doctorSkill checks every judge role's skill file (the legacy [codex]
// skill_path is only a fallback config.Load maps onto codex-judge's skill).
func doctorSkill(_ context.Context, d doctorDeps) []doctorCheck {
	var paths []string
	for _, r := range d.Config.RolesFor(nil) {
		if r.Judge && r.Skill != "" && !slices.Contains(paths, r.Skill) {
			paths = append(paths, r.Skill)
		}
	}
	if len(paths) == 0 {
		paths = []string{d.Config.JudgeFor(nil).Skill}
	}
	var out []doctorCheck
	for _, p := range paths {
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			out = append(out, doctorFailed("skill", "judge skill missing at "+inspTilde(p),
				"restore skills/magnum-review/SKILL.md (`git checkout -- skills`) or fix the judge role's skill (or [codex] skill_path) in config.toml"))
			continue
		}
		out = append(out, doctorOK("skill", "judge skill at "+inspTilde(p)))
	}
	return out
}

// doctorPromptKinds are the prompt kinds a role can name (config.Role.PromptFile).
var doctorPromptKinds = []string{config.PromptInitial, config.PromptRereview, config.PromptContinue,
	config.PromptRecovery, config.PromptNudge, config.PromptStop}

// doctorPrompts checks that [pipeline] prompts_dir exists when set and that
// every prompt file a role names resolves (prompts_dir, else the embedded
// copy).
func doctorPrompts(_ context.Context, d doctorDeps) []doctorCheck {
	var out []doctorCheck
	if dir := d.Config.Pipeline.PromptsDir; dir != "" {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			out = append(out, doctorWarned("prompts_dir", "prompts_dir "+inspTilde(dir)+" does not exist: every prompt comes from the embedded defaults",
				"create it (`git checkout -- prompts` in magnum's home) or fix [pipeline] prompts_dir in config.toml"))
		} else {
			out = append(out, doctorOK("prompts_dir", "prompts_dir "+inspTilde(dir)))
		}
	}
	seen := map[string]bool{}
	fromDir, embedded := 0, 0
	var missing []doctorCheck
	for _, r := range d.Config.RolesFor(nil) {
		for _, kind := range doctorPromptKinds {
			name := r.PromptFile(kind)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			p, err := d.Config.RolePrompt(r, kind)
			switch {
			case err != nil:
				missing = append(missing, doctorFailed("prompt "+name, fmt.Sprintf("role %s: %s prompt %s does not resolve: %v", r.Name, kind, name, err),
					"add "+name+" to prompts_dir, or fix the [[role]] "+r.Name+" prompt names in config.toml (`magnum roles` shows them)"))
			case p.Embedded:
				embedded++
			default:
				fromDir++
			}
		}
	}
	if len(missing) == 0 {
		out = append(out, doctorOK("prompts", fmt.Sprintf("all %d role prompts resolve (%d from prompts_dir, %d embedded)", fromDir+embedded, fromDir, embedded)))
	}
	return append(out, missing...)
}

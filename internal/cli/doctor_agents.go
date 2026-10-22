package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
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
	out, err := doctorShellProbe(ctx, d, 15*time.Second, "whence -w "+kind)
	for line := range strings.SplitSeq(out, "\n") {
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

// doctorKinds are the agent kinds doctor checks: the configured roles'
// (rolesKindNames), then the notes critic's when [notes] critic is on.
func doctorKinds(cfg *config.Config) []string {
	out := rolesKindNames(cfg)
	if k := cfg.CriticRole().AgentKind(); cfg.Notes.Critic && k != "" && k != config.KindShell && !slices.Contains(out, k) {
		out = append(out, k)
	}
	return out
}

// doctorKindUsers names what uses kind: the roles (rolesUsers), and the
// notes critic (config.CriticRoleName) when [notes] critic is on.
func doctorKindUsers(cfg *config.Config, kind string) []string {
	out := rolesUsers(cfg, kind)
	if cfg.Notes.Critic && cfg.CriticRole().AgentKind() == kind {
		out = append(out, config.CriticRoleName)
	}
	return out
}

// doctorAgentChecks checks every agent kind the configured roles use (a
// shell role's tool counts) and the notes critic's when it is on
// (doctorKinds): the CLI is on PATH in zsh, its login check passes
// ([kinds.<name>] login_check and login_ok), and how magnum passes its args
// (wrapper detection; a plain binary must get the kind's autonomy flag, see
// doctorPlainBinary).
func doctorAgentChecks(ctx context.Context, d doctorDeps) []doctorCheck {
	var out, wrappers []doctorCheck
	for _, kind := range doctorKinds(d.Config) {
		k, _ := d.Config.KindSpec(kind)
		users := strings.Join(doctorKindUsers(d.Config, kind), ", ")
		what, werr := doctorWhence(ctx, d, kind)
		onPath := werr == nil && what != "none"
		skipLogin := "" // why the login check cannot run
		switch {
		case werr != nil:
			skipLogin = "zsh did not say where " + kind + " is"
			out = append(out, doctorFailed(kind, fmt.Sprintf("could not ask zsh for %s (used by %s): %s", kind, users, werr.Error()),
				fmt.Sprintf("`zsh -ic 'whence -w %s' </dev/null` must answer within seconds; when it does not, something in your zsh startup files waits (for input, the network or the terminal)", kind)))
		case !onPath:
			skipLogin = kind + " is not on PATH"
			out = append(out, doctorFailed(kind, fmt.Sprintf("%s (used by %s) is not on PATH in zsh", kind, users), doctorInstall(kind)))
		default:
			label := kind
			if version, err := doctorExec(ctx, d, 15*time.Second, "", nil, kind, "--version"); err == nil && version != "" {
				label = kind + " " + textx.FirstLine(version)
			}
			out = append(out, doctorOK(kind, fmt.Sprintf("%s is on PATH (zsh %s; used by %s)", label, what, users)))
		}
		out = append(out, doctorLogin(ctx, d, kind, k, skipLogin))

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
// config.Kind.LoggedIn); skip says why a check that runs kind itself cannot
// run ("" = it can).
func doctorLogin(ctx context.Context, d doctorDeps, kind string, k config.Kind, skip string) doctorCheck {
	name := kind + " login"
	argv := k.LoginArgv()
	check := strings.Join(argv, " ")
	switch {
	case len(argv) == 0:
		return doctorSkipped(name, fmt.Sprintf("%s has no login check ([kinds.%s] login_check)", kind, kind))
	case skip != "" && argv[0] == kind:
		return doctorSkipped(name, fmt.Sprintf("`%s` not run: %s", check, skip))
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
		return doctorWarned(name, fmt.Sprintf("cannot read the output of `%s` with login_ok %q: %s", check, k.LoginOK, textx.FirstLine(execx.Redact(stdout+"\n"+stderr))),
			"fix [kinds."+kind+"] login_check or login_ok in config.toml (magnum assumes logged in meanwhile)")
	case !loggedIn:
		return doctorFailed(name, fmt.Sprintf("%s is logged out (`%s`)", kind, check), "run `"+rolesLoginCommand(kind)+"` (reviews pause until it passes)")
	}
	return doctorOK(name, fmt.Sprintf("%s is logged in (`%s`)", kind, check))
}

// doctorSessionIDWait is how old a Codex session must be for doctor to
// count it without a session id: herdr's hook reports the id at the start
// of its first turn.
const doctorSessionIDWait = 10 * time.Minute

// doctorSessionHooks checks herdr's Codex session hook for the codex kind
// when its session ids come from herdr (session_source "herdr") and a
// session role starts it (a shell role's `codex review` needs no id), one
// Codex home at a time of those the roles read (agents.CodexHookHomes over
// agents.Manager.CodexHome). herdr learns a session's id from its
// SessionStart hook in the home's hooks.json (agents.ReadCodexSessionHook).
// The "codex session hook" line warns when no SessionStart hook names
// herdr's script, when every such hook's [hooks.state] entry in config.toml
// has enabled = false, and when a hook before it has no timeout or one over
// agents.CodexHookMaxTimeout (its place); "codex session hook trust" warns
// when its place has no trust entry (Codex keys trust by place); the OK line
// comes only when every check passes and names its place and herdr's
// integration version. A home without hooks.json is skipped (herdr not
// installed is the herdr check's). Then "codex session ids" reads the
// registry (doctorSessionIDs). It only reads: the hooks and their trust are
// the operator's to change.
func doctorSessionHooks(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = config.KindCodex + " session hook"
	homes, users := agents.CodexHookHomes(d.Config, d.Agents.CodexHome)
	if len(homes) == 0 {
		return nil
	}
	var out []doctorCheck
	for _, home := range homes {
		used := "used by " + strings.Join(users[home], ", ")
		h, ok, err := agents.ReadCodexSessionHook(home)
		hooksFile, configFile := inspTilde(h.HooksFile), inspTilde(h.ConfigFile)
		if h.Version != "" {
			used += "; herdr integration " + h.Version
		}
		switch {
		case home == "":
			out = append(out, doctorSkipped(name, fmt.Sprintf("herdr's Codex session hook not checked: the Codex home of %s is unknown", strings.Join(users[home], ", "))))
			continue
		case err != nil:
			out = append(out, doctorWarned(name, fmt.Sprintf("cannot tell whether herdr's Codex session hook is on (%s): %s", used, inspTildeIn(err.Error(), home)),
				"fix the file: Codex reads it at every start"))
			continue
		case !ok:
			out = append(out, doctorSkipped(name, fmt.Sprintf("herdr's Codex session hook not checked: %s does not exist (herdr's Codex integration writes it)", hooksFile)))
			continue
		case !h.Found:
			out = append(out, doctorWarned(name, fmt.Sprintf("herdr's Codex session hook is missing: no SessionStart hook in %s names %s (%s): %s",
				hooksFile, agents.HerdrSessionHook, used, agents.SessionIDBlind),
				"reinstall herdr's Codex integration (`herdr integration` lists its commands)"))
			continue
		case len(h.Disabled) > 0:
			entries := make([]string, len(h.Disabled))
			for i, key := range h.Disabled {
				entries[i] = fmt.Sprintf("[hooks.state.%q]", key)
			}
			out = append(out, doctorWarned(name, fmt.Sprintf("herdr's Codex session hook is disabled: %s in %s has enabled = false (%s): %s",
				strings.Join(entries, ", "), configFile, used, agents.SessionIDBlind),
				fmt.Sprintf("set enabled = true in that entry of %s (Codex keeps your hook trust there; magnum never changes it)", configFile)))
			continue
		}
		problems := h.Problems(hooksFile, configFile)
		for _, p := range problems {
			switch p.Check {
			case agents.CodexHookPlace:
				out = append(out, doctorWarned(name, fmt.Sprintf("%s (%s): a slow hook can hold it up, and Codex sessions get no id", p.Text, used),
					fmt.Sprintf("move herdr's entry first in %s, or give those hooks a timeout of %d s or less; Codex keys a hook's trust by its place, "+
						"so trust the hooks again when Codex asks (magnum never edits the file)", hooksFile, agents.CodexHookMaxTimeout)))
			case agents.CodexHookTrust:
				out = append(out, doctorWarned(name+" trust", fmt.Sprintf("%s (%s): Codex asks you to review the hooks before it runs this one, "+
					"and until then Codex sessions get no id", p.Text, used),
					fmt.Sprintf("start `codex` in a terminal and trust herdr's hook when it asks you to review the hooks; Codex records that "+
						"under the hook's place in %s's [hooks.state] (magnum never changes it)", configFile)))
			}
		}
		if len(problems) == 0 {
			where := "first"
			if len(h.Before) > 0 {
				where = fmt.Sprintf("after %s with a timeout of %d s or less",
					textx.Count(len(h.Before), "SessionStart hook", "SessionStart hooks"), agents.CodexHookMaxTimeout)
			}
			out = append(out, doctorOK(name, fmt.Sprintf("herdr's Codex session hook is on (SessionStart %s at %s in %s, %s, trusted; %s): "+
				"Codex sessions report their ids, so judges resume", agents.HerdrSessionHook, h.Place, hooksFile, where, used)))
		}
	}
	return append(out, doctorSessionIDs(ctx, d)...)
}

// doctorSessionIDs reads the registry: of the Codex sessions of the last
// day that ran an agent and are at least doctorSessionIDWait old
// (agents.RecentSessionIDs), it warns when the newest two or more in a row
// have no session id, whatever the hook checks say (herdr's script can fail
// too), counting those without one since two in a row last had one
// (agents.MissingSessionIDs); it passes with how many have one, and skips
// when none ran.
func doctorSessionIDs(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = config.KindCodex + " session ids"
	if d.Store == nil {
		return nil
	}
	now := time.Now()
	sessions, err := agents.RecentSessionIDs(ctx, d.Store, config.KindCodex, now, doctorSessionIDWait)
	if err != nil {
		return []doctorCheck{doctorWarned(name, "cannot read the Codex sessions' ids from the registry: "+err.Error(), "check "+inspTilde(d.Layout.DB()))}
	}
	if len(sessions) == 0 {
		return []doctorCheck{doctorSkipped(name, fmt.Sprintf("no Codex session older than %s ran an agent in the last 24 hours", textx.DurationWords(doctorSessionIDWait)))}
	}
	recovered := doctorRolloutIDs(ctx, d, now.Add(-agents.SessionIDWindow))
	with, fromRollout := 0, 0
	for _, s := range sessions {
		if agents.HasSessionID(s) {
			with++
			if recovered[s.ID] {
				fromRollout++
			}
		}
	}
	rollouts := ""
	if fromRollout > 0 {
		rollouts = fmt.Sprintf("; %s Codex rollout%s, as herdr's hook gave none",
			textx.Count(fromRollout, "id came from its session's", "ids came from their sessions'"), textx.Plural(fromRollout, "", "s"))
	}
	gap := agents.MissingSessionIDs(sessions)
	if gap.Streak >= 2 {
		return []doctorCheck{doctorWarned(name, fmt.Sprintf("%d of the %d Codex sessions started since %s have no session id, the newest %d in a row%s: %s",
			gap.Missing, gap.Sessions, gap.Since.Local().Format("15:04"), gap.Streak, rollouts, agents.SessionIDBlind),
			"the codex session hook lines say what is wrong with herdr's hook; when they pass, herdr's hook script may give no id: "+
				"reinstall herdr's Codex integration (`herdr integration` lists its commands)")}
	}
	return []doctorCheck{doctorOK(name, fmt.Sprintf("%d of the %d Codex sessions that ran an agent in the last 24 hours have a session id%s",
		with, len(sessions), rollouts))}
}

// doctorRolloutIDs are the sessions (row ids) whose session id the
// observer took from their Codex rollouts since since (agent.session_id
// events, agents.EventSessionID): herdr's hook gave them none.
func doctorRolloutIDs(ctx context.Context, d doctorDeps, since time.Time) map[int64]bool {
	evs, err := d.Store.EventsOfKindsSince(ctx, since, agents.EventSessionID)
	if err != nil {
		return nil
	}
	out := map[int64]bool{}
	for _, ev := range evs {
		var data struct {
			Session int64  `json:"session"`
			Source  string `json:"source"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && data.Source == agents.SessionIDFromRollout {
			out[data.Session] = true
		}
	}
	return out
}

// inspTildeIn shortens the Codex home home to ~ where s names it.
func inspTildeIn(s, home string) string {
	return strings.ReplaceAll(s, home, inspTilde(home))
}

// doctorLearnEditor says whether the skill editor can run, and why it is
// off: [learn] editor itself, no checkout of magnum to edit (Layout.Home: an
// installed binary has none) or go that does not run in the checkout's mise
// environment, where the format gate runs `go test` (`mise -C <checkout>
// exec -- go version`, as the daemon checks before a scheduled edit). It
// fails only when [learn] editor is on and the editor cannot run.
func doctorLearnEditor(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = "learn editor"
	home := d.Layout.Home
	var why, fix, version string
	switch {
	case home == "":
		why, fix = "magnum runs without a checkout of its repository (an installed binary has none)",
			"run magnum from a checkout of its repository (MAGNUM_HOME=<checkout>), or set [learn] editor = false"
	case !gitx.IsRepo(home):
		why, fix = inspTilde(home)+" is not a git checkout of magnum", "run magnum from a checkout of its repository, or set [learn] editor = false"
	default:
		out, err := doctorExec(ctx, d, time.Minute, home, nil, "mise", "-C", home, "exec", "--", "go", "version")
		if err != nil {
			why = fmt.Sprintf("go does not run in the checkout's mise environment (`mise -C %s exec -- go version`: %s)", inspTilde(home), err.Error())
			fix = fmt.Sprintf("install the checkout's go (`mise -C %s install`), or set [learn] editor = false", inspTilde(home))
		} else {
			version = textx.FirstLine(out)
		}
	}
	switch {
	case !d.Config.Learn.Editor && why != "":
		return []doctorCheck{doctorSkipped(name, "the skill editor is off ([learn] editor = false), and `magnum learn --propose` cannot run it either: "+why)}
	case !d.Config.Learn.Editor:
		return []doctorCheck{doctorSkipped(name, fmt.Sprintf("the skill editor is off ([learn] editor = false); `magnum learn --propose` runs it on request (checkout %s, %s)",
			inspTilde(home), version))}
	case why != "":
		return []doctorCheck{doctorFailed(name, "[learn] editor is on, but the skill editor cannot run, so it is off: "+why, fix)}
	}
	return []doctorCheck{doctorOK(name, fmt.Sprintf("the skill editor runs after a retro that stored new general misses or declines, at most once a day (checkout %s, %s)",
		inspTilde(home), version))}
}

// doctorSkill checks every judge role's skill file.
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
				"restore skills/magnum-review/SKILL.md (`git checkout -- skills`) or fix the judge role's skill in config.toml"))
			continue
		}
		out = append(out, doctorOK("skill", "judge skill at "+inspTilde(p)))
	}
	return out
}

// doctorFixRole checks the fix role of every watch (Config.FixRoleFor, the
// watch's poll identity when the role names none) and prints one "fix role"
// line per role and identity that says whether the fix job can run: its
// skill resolves (the checkout's files, else the binary's own copy), and its
// identity is a gh identity whose login check passes, since the fixer pushes
// and replies as the operator's own account. A watch whose autofix = "own"
// cannot start the job by itself fails: its fix_role names no declared fix
// role (a [[role]] list of the operator's own replaces the built-in
// claude-fix), or the poll identity its fix role takes is an App. A
// configuration that uses no fix job gets a SKIP: no fix role (fix_role = ""
// switches it off on purpose), a fix_role that names no declared fix role, an
// identity it never declared, or a watch that polls as an App, where no
// watch's autofix = "own" would start the job.
func doctorFixRole(ctx context.Context, d doctorDeps) []doctorCheck {
	const name = "fix role"
	cfg := d.Config
	var roles []config.Role
	var out []doctorCheck
	same := func(a, b config.Role) bool { return a.Name == b.Name && a.Identity == b.Identity }
	autofix := map[[2]string]bool{}       // by role and identity: a watch with autofix = "own" starts that job by itself
	owners := map[[2]string][]string{}    // by role and identity: the watches whose fix job it is
	var undeclared []string               // fix_role names that resolve to no declared fix role, where no autofix starts the job
	undeclaredBy := map[string][]string{} // by such a name: the watches (or [pipeline]) that name it
	add := func(w *config.Watch) {
		own := w != nil && cfg.AutofixFor(w) == config.AutofixOwn
		r, ok := cfg.FixRoleFor(w)
		if !ok {
			switch n := strings.TrimSpace(cfg.FixRoleName(w)); {
			case n == "":
			case own:
				out = append(out, doctorFailed(name, fmt.Sprintf(`watch %s: autofix = "own" but fix_role %q names no declared fix role, so the fix job never starts by itself`, w.Owner, n),
					fmt.Sprintf("declare a [[role]] named %s with fix = true (a [[role]] list of your own replaces the built-in claude-fix), or set fix_role", n)))
			default:
				if !slices.Contains(undeclared, n) {
					undeclared = append(undeclared, n)
				}
				by := "[pipeline]"
				if w != nil {
					by = "watch " + w.Owner
				}
				undeclaredBy[n] = append(undeclaredBy[n], by)
			}
			return
		}
		if !slices.ContainsFunc(roles, func(x config.Role) bool { return same(x, r) }) {
			roles = append(roles, r)
		}
		k := [2]string{r.Name, r.Identity}
		autofix[k] = autofix[k] || own
		if w != nil {
			owners[k] = append(owners[k], w.Owner)
		}
	}
	if len(cfg.Watches) == 0 {
		add(nil)
	}
	for i := range cfg.Watches {
		add(&cfg.Watches[i])
	}
	for _, n := range undeclared {
		out = append(out, doctorSkipped(name, fmt.Sprintf("fix_role %q names no declared fix role (a [[role]] list of your own replaces the built-in claude-fix), so `magnum fix` is refused for %s",
			n, strings.Join(undeclaredBy[n], ", "))))
	}
	if len(roles) == 0 && len(out) == 0 {
		return []doctorCheck{doctorSkipped(name, "no fix role: [pipeline] fix_role names none, so `magnum fix` is refused (claude-fix is the built-in one)")}
	}
	for _, r := range roles {
		k := [2]string{r.Name, r.Identity}
		out = append(out, doctorFixRoleCheck(ctx, d, r, autofix[k], owners[k])...)
	}
	return out
}

// doctorFixRoleCheck is doctorFixRole for one role and the identity it acts
// as for the watches owners; autofix says that one of them starts its job by
// itself, so a job that cannot run fails instead of being skipped.
func doctorFixRoleCheck(ctx context.Context, d doctorDeps, r config.Role, autofix bool, owners []string) []doctorCheck {
	const name = "fix role"
	head := "fix role " + r.Name
	if r.Identity == "" {
		return []doctorCheck{doctorSkipped(name, head+": no watch, so no poll identity to act as; `magnum fix` needs a [[watch]]")}
	}
	id := d.Config.IdentityByName(r.Identity)
	if id == nil {
		why := fmt.Sprintf("%s: identity %q is not declared, so the fix job cannot run", head, r.Identity)
		if autofix {
			return []doctorCheck{doctorFailed(name, why+`, and autofix = "own" would start it`,
				fmt.Sprintf("declare a gh [[identity]] named %q for your own account, or set identity in a [[role]] named %s", r.Identity, r.Name))}
		}
		return []doctorCheck{doctorSkipped(name, why+"; `magnum fix` is refused until it is declared")}
	}
	if id.Kind != "gh" && !autofix { // a poll identity the role took: polling as an App is a valid config
		return []doctorCheck{doctorSkipped(name, fmt.Sprintf("%s: `magnum fix` refuses: %s %s as an App (identity %s), the account the fixer would act as",
			head, textx.Plural(len(owners), "watch", "watches")+" "+strings.Join(owners, ", "), textx.Plural(len(owners), "polls", "poll"), id.Name))}
	}
	var out []doctorCheck
	var ok []string
	src := config.RoleSkillPath(r, d.Layout)
	fi, statErr := os.Stat(src)
	switch {
	case src == config.EmbeddedFixSkill:
		ok = append(ok, "skill is the binary's own copy")
	case statErr != nil || fi.IsDir():
		out = append(out, doctorFailed(name, head+": fix skill missing at "+inspTilde(src),
			"restore skills/magnum-fix/SKILL.md (`git checkout -- skills`) or fix the fix role's skill in config.toml"))
	default:
		ok = append(ok, "skill at "+inspTilde(src))
	}
	if p, err := d.Config.RolePrompt(r, config.PromptInitial); err != nil {
		out = append(out, doctorFailed(name, fmt.Sprintf("%s: prompt does not resolve: %v", head, err),
			"add the prompt to prompts_dir (`git checkout -- prompts`), or fix the [[role]] "+r.Name+" prompt in config.toml"))
	} else if p.Embedded {
		ok = append(ok, "prompt "+p.Name+" (embedded)")
	} else {
		ok = append(ok, "prompt "+p.Name+" (prompts_dir)")
	}
	who := fmt.Sprintf("identity %s (%s, %s)", id.Name, id.Kind, id.Login)
	kv := func(key string) string {
		if d.Store == nil {
			return ""
		}
		v, _, _ := d.Store.GetKV(ctx, key)
		return v
	}
	switch {
	case id.Kind != "gh":
		out = append(out, doctorFailed(name, fmt.Sprintf(`%s: identity %s is an %s identity: the fixer pushes and replies as your own account, and autofix = "own" would start it`, head, id.Name, id.Kind),
			"poll as a gh [[identity]] for your account (the watch's poll_identity, which the fix role takes), or set identity in the [[role]] named "+r.Name))
	case kv(store.KVIdentityCheck(id.Name)) == "pass":
		ok = append(ok, who+": last check passed")
	case kv(store.KVIdentityCheck(id.Name)) == "fail":
		out = append(out, doctorFailed(name, fmt.Sprintf("%s: %s: last check failed: %s", head, who, kv(store.KVIdentityError(id.Name))),
			"fix it, then `magnum identities check --name "+id.Name+"`"))
	default:
		out = append(out, doctorWarned(name, head+": "+who+": never checked", "magnum identities check --name "+id.Name))
	}
	if len(out) == 0 {
		return []doctorCheck{doctorOK(name, head+": "+strings.Join(ok, ", "))}
	}
	return out
}

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
		for _, kind := range config.PromptKinds {
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

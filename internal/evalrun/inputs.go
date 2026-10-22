package evalrun

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
)

// Inputs is a hash (12 hex) of what a replay's review reads besides the PR
// and the notes (eval.Run.Inputs): every role's config with the model it
// resolves to and its kind's config, the prompt files the roles name as they
// resolve now (the checkout's prompts_dir, else the embedded ones), with the
// judge prompts' partials file (config.SharedPrompt) their blocks come from, each
// judge's skill file, the stakes lines of the [[watch]] and [[repo]] blocks
// (what the judge and the reviewers size a harm against), and the
// simplification caps the judge posts by: the pipeline table's
// max_simplifications in effect and each [[repo]] block's that is set
// (config.Config.MaxSimplificationsFor), and with [jira] on the site and the
// account the judge reads the PR's ticket from (not the token file). The
// texts count, not where they are: a judge's skill path is left out of its
// role config, so a candidate directory with the same texts as the checkout
// has its inputs (a landed candidate's replays are the next baseline).
// magnum's own code is not part of it. "" without a config.
func Inputs(cfg *config.Config) string {
	return inputsOf(cfg, func(config.Role) bool { return true }, true)
}

// ReviewerInputs is the part of Inputs the reviewer roles read
// (eval.Run.ReviewerInputs): the same hash over the roles that are not
// judges, with the stakes lines the reviewers read too and without the
// simplification caps and [jira], which only the judges read, so a change to
// a judge's prompts, skill, caps or ticket leaves it alone and a judge-only
// replay can take the reports of a full replay whose reviewers read what its
// own would (eval.FullReplayFor). "" without a config.
func ReviewerInputs(cfg *config.Config) string {
	return inputsOf(cfg, func(r config.Role) bool { return !r.Judge }, false)
}

// inputsOf hashes the inputs of the roles of cfg that of takes, then the
// stakes lines, then, with judges, the simplification caps and [jira].
func inputsOf(cfg *config.Config, of func(config.Role) bool, judges bool) string {
	if cfg == nil {
		return ""
	}
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			fmt.Fprintf(h, "%d:%s\n", len(p), p)
		}
	}
	for _, r := range cfg.RolesFor(nil) {
		if !of(r) {
			continue
		}
		at := r
		at.Skill = "" // its text is hashed below
		role, _ := json.Marshal(at)
		kind, _ := json.Marshal(cfg.Kinds[r.Kind])
		write("role", string(role), cfg.RoleModel(r), string(kind))
		shared := ""
		for _, k := range config.PromptKinds {
			name := r.PromptFile(k)
			if name == "" {
				continue
			}
			text := "(missing)"
			if p, err := cfg.ResolvePrompt(name); err == nil {
				text, shared = p.Text, cmp.Or(shared, p.Shared)
			}
			write("prompt", k, name, text)
		}
		if shared != "" {
			// The blocks the role's judge prompts include; a role without
			// one (every reviewer) hashes as before the file existed.
			write("prompt", "shared", config.SharedPrompt, shared)
		}
		if r.Judge {
			write("skill", skillText(config.SkillPath(r.Skill, cfg.Layout)))
		}
	}
	for _, l := range stakesLines(cfg) {
		write("stakes", l.key, l.line)
	}
	if judges {
		// A repository without a block of its own posts by the pipeline table's cap.
		write("max_simplifications", "pipeline", strconv.Itoa(cfg.MaxSimplificationsFor("")))
		for _, l := range simplificationCaps(cfg) {
			write("max_simplifications", l.key, l.line)
		}
		// The judge reads the PR's Jira ticket from this site as this
		// account; with [jira] off nothing is written, so such a config
		// hashes as before the ticket counted. The token file is no input.
		if cfg.Jira.On() {
			write("jira", cfg.Jira.URL, cfg.Jira.User)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// blockLine is a value of a config block with the block it comes from.
type blockLine struct{ key, line string }

// stakesLines are the stakes lines of cfg's [[watch]] and [[repo]] blocks
// that are set (config.StakesFor reads only those), each with the block's
// kind and name in lower case, the way the blocks are looked up, sorted by
// that key so the order in the file is not an input (byKey). A config that
// sets none has none, so its hash is what it was before the lines counted.
func stakesLines(cfg *config.Config) []blockLine {
	var out []blockLine
	for _, w := range cfg.Watches {
		if w.Stakes != "" {
			out = append(out, blockLine{"watch " + strings.ToLower(w.Owner), w.Stakes})
		}
	}
	for _, r := range cfg.Repos {
		if r.Stakes != "" {
			out = append(out, blockLine{"repo " + strings.ToLower(r.Repo), r.Stakes})
		}
	}
	return byKey(out)
}

// simplificationCaps are the max_simplifications of cfg's [[repo]] blocks
// that set one (0 is the pipeline table's, which inputsOf hashes on its
// own), keyed and sorted as stakesLines are.
func simplificationCaps(cfg *config.Config) []blockLine {
	var out []blockLine
	for _, r := range cfg.Repos {
		if r.MaxSimplifications > 0 {
			out = append(out, blockLine{"repo " + strings.ToLower(r.Repo), strconv.Itoa(r.MaxSimplifications)})
		}
	}
	return byKey(out)
}

// byKey sorts lines by their key; two blocks with one key (two watches of
// an owner) keep their order in the file.
func byKey(lines []blockLine) []blockLine {
	slices.SortStableFunc(lines, func(a, b blockLine) int { return strings.Compare(a.key, b.key) })
	return lines
}

// skillText is the text of the judge skill at src (config.SkillPath).
func skillText(src string) string {
	if src == config.EmbeddedSkill {
		return string(magnum.Skill)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return "(unreadable)"
	}
	return string(b)
}

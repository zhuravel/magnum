package evalrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
)

// Inputs is a hash (12 hex) of what a replay's review reads besides the PR
// and the notes (eval.Run.Inputs): every role's config with the model it
// resolves to and its kind's config, the prompt files the roles name as they
// resolve now (the checkout's prompts_dir, else the embedded ones), each
// judge's skill file, and the stakes lines of the [[watch]] and [[repo]]
// blocks (what the judge and the reviewers size a harm against). The texts
// count, not where they are: a judge's skill path is left out of its role
// config, so a candidate directory with the same texts as the checkout has
// its inputs (a landed candidate's replays are the next baseline). magnum's
// own code is not part of it. "" without a config.
func Inputs(cfg *config.Config) string {
	return inputsOf(cfg, func(config.Role) bool { return true })
}

// ReviewerInputs is the part of Inputs the reviewer roles read
// (eval.Run.ReviewerInputs): the same hash over the roles that are not
// judges, with the stakes lines the reviewers read too, so a change to a
// judge's prompts or skill leaves it alone and a judge-only replay can take
// the reports of a full replay whose reviewers read what its own would
// (eval.FullReplayFor). "" without a config.
func ReviewerInputs(cfg *config.Config) string {
	return inputsOf(cfg, func(r config.Role) bool { return !r.Judge })
}

// inputsOf hashes the inputs of the roles of cfg that of takes, then the
// stakes lines.
func inputsOf(cfg *config.Config, of func(config.Role) bool) string {
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
		for _, k := range config.PromptKinds {
			name := r.PromptFile(k)
			if name == "" {
				continue
			}
			text := "(missing)"
			if p, err := cfg.ResolvePrompt(name); err == nil {
				text = p.Text
			}
			write("prompt", k, name, text)
		}
		if r.Judge {
			write("skill", skillText(config.SkillPath(r.Skill, cfg.Layout)))
		}
	}
	for _, l := range stakesLines(cfg) {
		write("stakes", l.key, l.line)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// stakesLine is a stakes line with the block it comes from.
type stakesLine struct{ key, line string }

// stakesLines are the stakes lines of cfg's [[watch]] and [[repo]] blocks
// that are set (config.StakesFor reads only those), each with the block's
// kind and name in lower case, the way the blocks are looked up, sorted by
// that key so the order in the file is not an input. Two blocks with one key
// (two watches of an owner) keep their order in the file. A config that sets
// none has none, so its hash is what it was before the lines counted.
func stakesLines(cfg *config.Config) []stakesLine {
	var out []stakesLine
	for _, w := range cfg.Watches {
		if w.Stakes != "" {
			out = append(out, stakesLine{"watch " + strings.ToLower(w.Owner), w.Stakes})
		}
	}
	for _, r := range cfg.Repos {
		if r.Stakes != "" {
			out = append(out, stakesLine{"repo " + strings.ToLower(r.Repo), r.Stakes})
		}
	}
	slices.SortStableFunc(out, func(a, b stakesLine) int { return strings.Compare(a.key, b.key) })
	return out
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

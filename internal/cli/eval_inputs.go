package cli

// What a replay records besides its score, so that runs can be compared and
// reused: the inputs it reviewed with (the judge skill, the roles' prompts
// and the role config, as one hash), the notes its judge read (a hash per
// file) and the Codex points it used (the gauge before and after the case).
// `magnum eval baseline` lists the stored replays a new run with the same
// inputs would repeat; it reads, it never replays.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/eval"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/usage"
)

const evalBaselineUsage = "[--corpus FILE] [--case NAME]..."

// evalInputs is a hash (12 hex) of what a replay's review reads besides the
// PR and the notes (eval.Run.Inputs): every role's config with the model it
// resolves to and its kind's config, the prompt files the roles name as they
// resolve now (the checkout's prompts_dir, else the embedded ones), and each
// judge's skill file. magnum's own code is not part of it. "" without a
// config.
func evalInputs(cfg *config.Config) string {
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
		role, _ := json.Marshal(r)
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
			write("skill", evalSkillText(config.SkillPath(r.Skill, cfg.Layout)))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// evalSkillText is the text of the judge skill at src (config.SkillPath).
func evalSkillText(src string) string {
	if src == config.EmbeddedSkill {
		return string(magnum.Skill)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return "(unreadable)"
	}
	return string(b)
}

// evalNotesHashes is what notes a case's judge reads: the hashes of the
// scratch copy of owner/repo's notes (eval.HashNotes), taken before the
// round can rewrite them; nil when they cannot be read.
func evalNotesHashes(scratch paths.Layout, owner, repo string) map[string]string {
	file := engine.NotesPath(scratch, owner, repo)
	if file == "" {
		return nil
	}
	m, err := eval.HashNotes(file, engine.NotesDir(scratch, owner, repo))
	if err != nil {
		return nil
	}
	return m
}

// evalCodexUsed is the Codex budget used now (usage.Codex: the newest
// snapshot of the account's sessions), nil when there is none to read.
func evalCodexUsed(ctx context.Context, cfg *config.Config) *float64 {
	snap, err := usage.Codex(ctx, cfg.Usage.CodexHome, time.Now())
	if err != nil {
		return nil
	}
	used := snap.Used()
	return &used
}

func newEvalBaselineCmd(c *Context) *cobra.Command {
	var corpus string
	var cases []string
	cmd := newCommand("", "baseline "+evalBaselineUsage, "the stored replays a run of these cases would repeat",
		"For each case of the corpus (or each --case), list the stored runs that replayed it at the head the corpus "+
			"pins, from a clean checkout, with the inputs this binary and config give a review now (the judge skill, "+
			"the roles' prompts and the role config; run.json's inputs), newest first. A new replay of such a case "+
			"repeats one of them unless magnum's code changed since its commit (eval-at.sh compares the Go files). "+
			"It only reads: no replay starts.\n\n"+
			"Output, tab-separated: `inputs <hash>`, then per case one `reuse <case> <run> <commit> <found>/<total>` "+
			"line per stored run, or `run <case>` when there is none.",
		func(pos []string) int {
			if len(pos) > 0 {
				return inspUsage(c, "eval baseline", "no arguments expected", evalBaselineUsage)
			}
			return runEvalBaseline(c, corpus, cases)
		})
	fs := cmd.Flags()
	fs.StringVar(&corpus, "corpus", "", "corpus file (default: "+evalCorpusFile+" next to config.toml)")
	fs.StringArrayVar(&cases, "case", nil, "only this case (repeatable)")
	return cmd
}

func runEvalBaseline(c *Context, corpusFlag string, names []string) int {
	if err := c.LoadConfig(); err != nil {
		return cmdFail(c, "eval baseline", fmt.Errorf("%w (fix config.toml; `magnum config` validates it)", err))
	}
	corpus, err := eval.LoadCorpus(evalCorpusPath(c, corpusFlag))
	if err != nil {
		return cmdFail(c, "eval baseline", err)
	}
	cases, err := corpus.Select(names)
	if err != nil {
		return cmdFail(c, "eval baseline", err)
	}
	runs, err := eval.ListRuns(evalRunsRoot(c.Layout))
	if err != nil {
		return cmdFail(c, "eval baseline", err)
	}
	inputs := evalInputs(c.Config)
	fmt.Fprintf(c.Stdout, "inputs\t%s\n", inputs)
	for _, ec := range cases {
		found := eval.Baselines(runs, inputs, ec)
		if len(found) == 0 {
			fmt.Fprintf(c.Stdout, "run\t%s\n", ec.Name)
			continue
		}
		for _, b := range found {
			commit := b.Commit
			if commit == "" {
				commit = "-"
			}
			fmt.Fprintf(c.Stdout, "reuse\t%s\t%s\t%s\t%d/%d\n", ec.Name, b.Run, commit, b.Case.Score.Found, b.Case.Score.Total)
		}
	}
	return 0
}

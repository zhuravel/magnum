package evalrun

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/config"
)

// CandidateSkill is where a candidate directory holds the judge skill, as
// magnum's checkout does.
const CandidateSkill = "skills/magnum-review/SKILL.md"

// WithCandidate is the config a replay of candidate dir reviews with: a
// copy of cfg (config.Config.WithoutSnapshot: read from disk, whatever the
// daemon loaded at startup) whose prompts_dir is <dir>/prompts and whose
// every judge reads <dir>/skills/magnum-review/SKILL.md, both absolute. dir
// is a checkout or a worktree of magnum with a change to the prompts or the
// skill; a prompt it lacks resolves to the embedded default, as in any
// checkout. An error when dir has no prompts directory or no skill file;
// cfg is never changed.
func WithCandidate(cfg *config.Config, dir string) (*config.Config, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("candidate %s: %w", dir, err)
	}
	prompts, skill := filepath.Join(abs, "prompts"), filepath.Join(abs, CandidateSkill)
	if st, err := os.Stat(prompts); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("candidate %s: no prompts directory at %s", dir, prompts)
	}
	if st, err := os.Stat(skill); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("candidate %s: no judge skill at %s", dir, skill)
	}
	out := cfg.WithoutSnapshot()
	out.Pipeline.PromptsDir = prompts
	for i := range out.Roles {
		if out.Roles[i].Judge {
			out.Roles[i].Skill = skill
		}
	}
	return out, nil
}

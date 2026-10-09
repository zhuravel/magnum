package evalrun

import (
	"github.com/zhuravel/magnum/internal/config"
)

// CandidateSkill is where a candidate directory holds the judge skill, as
// magnum's checkout does.
const CandidateSkill = config.CandidateSkill

// WithCandidate is the config a replay of candidate dir reviews with
// (config.Config.WithCandidate, which the skill editor's checks share): a
// copy of cfg (config.Config.WithoutSnapshot: read from disk, whatever the
// daemon loaded at startup) whose prompts_dir is <dir>/prompts and whose
// every judge reads <dir>/skills/magnum-review/SKILL.md, both absolute. dir
// is a checkout or a worktree of magnum with a change to the prompts or the
// skill; a prompt it lacks resolves to the embedded default, as in any
// checkout. An error when dir has no prompts directory or no skill file;
// cfg is never changed.
func WithCandidate(cfg *config.Config, dir string) (*config.Config, error) {
	return cfg.WithCandidate(dir)
}

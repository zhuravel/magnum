package factory

// The review candidate (spec 6.5): the factory's local rounds run magnum's
// review with this build's own prompts and judge skill, one prompt swapped,
// as a candidate directory (config.WithCandidate) the live rounds never
// read: claude-review.md is the local reviewer prompt, which reviews the
// change in the copy instead of `/code-review <PR URL>`. The judge prompts
// carry their local block behind `{{if .Local}}`, so the candidate's judge
// prompts are the live ones.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/prompts"
)

// LocalReviewPrompt is the local reviewer prompt, the candidate's
// claude-review.md; LiveReviewPrompt the prompt it stands in for. They are
// evalrun's, whose RunLocal gives every local round, the factory's and a
// replay's of a local case alike, the local prompt in the live one's place
// (evalrun.LocalReviewConfig): the swap here is the same text again.
const (
	LocalReviewPrompt = evalrun.LocalReviewPrompt
	LiveReviewPrompt  = evalrun.LiveReviewPrompt
)

// ReviewCandidateDir is where the factory writes its review candidate:
// <data>/factory/review-candidate/.
func ReviewCandidateDir(l paths.Layout) string {
	return filepath.Join(l.Data(), "factory", "review-candidate")
}

// SkillCopyDir is where the factory daemon copies its fixer's skill at its
// start (config.Config.SnapshotFactoryPrompts): <data>/factory/skill/, its
// own, apart from the review daemon's copies (engine.SkillCopyDir), since
// each prunes the copies its own roles do not use.
func SkillCopyDir(l paths.Layout) string {
	return filepath.Join(l.Data(), "factory", "skill")
}

// WriteReviewCandidate writes the review candidate the factory's local
// rounds run with (their config is cfg.WithCandidate(dir)) and returns its
// directory: prompts/ with every prompt this build embeds, the partials
// files included, LocalReviewPrompt as claude-review.md, and the build's
// judge skill at skills/magnum-review/SKILL.md (magnum.Skill). The factory
// daemon writes it once, at startup, as it loads its own prompts: whatever
// an earlier build left there goes. Magnum's live rounds never read it.
func WriteReviewCandidate(l paths.Layout) (string, error) {
	dir := ReviewCandidateDir(l)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("factory: review candidate: %w", err)
	}
	write := func(rel string, b []byte) error {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return fsx.WriteFileAtomic(path, b, 0o600)
	}
	for _, name := range append(prompts.Names(), prompts.Shared, prompts.FactoryShared) {
		src := name
		if name == LiveReviewPrompt {
			src = LocalReviewPrompt
		}
		text, ok := prompts.Read(src)
		if !ok {
			return "", fmt.Errorf("factory: review candidate: this build embeds no %s", src)
		}
		if err := write(filepath.Join("prompts", name), []byte(text)); err != nil {
			return "", fmt.Errorf("factory: review candidate: %w", err)
		}
	}
	if err := write(config.CandidateSkill, magnum.Skill); err != nil {
		return "", fmt.Errorf("factory: review candidate: %w", err)
	}
	return dir, nil
}

// skillHash is the hash of the review skill a local round's judge reads:
// the sha256 of the candidate's skill file, "" when it cannot be read.
func skillHash(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, config.CandidateSkill))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

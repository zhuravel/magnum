package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/paths"
)

// PromptSnapshot is the prompt text and judge skills a daemon loaded once, at
// startup (Config.SnapshotPrompts), right after it checked that this build
// renders them. While a Config holds one, ResolvePrompt answers from it and
// SkillFile names the skill's copy, so a file edited on disk reaches rounds
// at the next restart, the one that checks it again (DECISIONS "Prompts are
// loaded once, at startup"). Changed tells which files differ on disk since.
// The CLI never takes one: `magnum config` and doctor read the files on disk.
// A snapshot is immutable once installed.
type PromptSnapshot struct {
	LoadedAt time.Time
	// Warnings name the skills that could not be copied (a missing or
	// unreadable file): their judges keep the configured path.
	Warnings []string

	dir     string                // pipeline.prompts_dir when loaded
	prompts map[string]snapPrompt // by prompt name
	skills  map[string]snapSkill  // by the configured skill path (SkillPath)
}

type snapPrompt struct {
	p     Prompt
	stamp fileStamp // the file read; zero for an embedded default
}

type snapSkill struct {
	copy  string    // <skill dir>/<hash>/SkillCopyName
	stamp fileStamp // the configured file when it was copied
}

// fileStamp identifies a file's content cheaply: same size and modification
// time means unchanged without reading it; otherwise the hash decides.
type fileStamp struct {
	size     int64
	mod      time.Time
	sum      [sha256.Size]byte
	embedded bool // the binary's own copy: never changes under a running daemon
}

// readStamped reads the file at path and stamps it.
func readStamped(path string) ([]byte, fileStamp, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fileStamp{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fileStamp{}, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fileStamp{}, err
	}
	return b, fileStamp{size: fi.Size(), mod: fi.ModTime(), sum: sha256.Sum256(b)}, nil
}

// same reports whether path still holds the content st stamped.
func (st fileStamp) same(path string) bool {
	if st.embedded {
		return true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if fi.Size() == st.size && fi.ModTime().Equal(st.mod) {
		return true
	}
	b, err := os.ReadFile(path)
	return err == nil && sha256.Sum256(b) == st.sum
}

// SkillCopyName is the file name of a judge skill's startup copy
// (<skill dir>/<first 12 hex of its SHA-256>/SKILL.md).
const SkillCopyName = "SKILL.md"

// skillCopyKeep is how long a copy no current judge uses is kept: a judge
// abandoned by the restart may still be reading the one it was given.
const skillCopyKeep = 7 * 24 * time.Hour

var skillCopyDirRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// EmbeddedSkill names the binary's own copy of the judge skill
// (magnum.Skill) as a skill source: what a judge gets without a checkout.
const EmbeddedSkill = "builtin:skills/magnum-review/SKILL.md"

// SkillPath is a judge's skill as a round names it: skill with {{repo}}
// replaced by layout's checkout, or layout.Skill() when skill is ""; the
// embedded skill (EmbeddedSkill) when either needs a checkout the layout
// lacks (Load expands {{repo}} and ~ already; Defaults keeps them).
func SkillPath(skill string, layout paths.Layout) string {
	switch {
	case skill == "" && layout.Skill() != "":
		return layout.Skill()
	case skill == "":
		return EmbeddedSkill
	case strings.Contains(skill, "{{repo}}") && layout.Home == "":
		return EmbeddedSkill
	case strings.Contains(skill, "{{repo}}"):
		return strings.ReplaceAll(skill, "{{repo}}", layout.Home)
	}
	return skill
}

// SnapshotPrompts loads every prompt file the roles name (each prompt kind)
// and the extra names (model-fallback.md), copies each judge's skill file
// to skillDir/<hash>/SKILL.md ("" = no copies; the judges keep their
// configured skill paths), removes copies older than a week that no judge
// uses, and installs the result: from now on ResolvePrompt answers from it
// and SkillFile names the copies. A prompt that does not resolve is an
// error and nothing is installed; a skill that cannot be copied is a
// warning (PromptSnapshot.Warnings). Call it once, before the goroutines
// that resolve prompts start.
func (c *Config) SnapshotPrompts(skillDir string, now time.Time, extra ...string) (*PromptSnapshot, error) {
	s := &PromptSnapshot{LoadedAt: now, dir: c.Pipeline.PromptsDir, prompts: map[string]snapPrompt{}, skills: map[string]snapSkill{}}
	var names []string
	add := func(n string) {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	for _, r := range c.Roles {
		for _, kind := range PromptKinds {
			add(r.PromptFile(kind))
		}
	}
	for _, n := range extra {
		add(n)
	}
	var errs []error
	for _, name := range names {
		if err := validPromptName(name); err != nil {
			errs = append(errs, err)
			continue
		}
		p, st, err := c.readPrompt(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.prompts[name] = snapPrompt{p: p, stamp: st}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config: load prompts: %w", err)
	}
	if skillDir != "" {
		for _, r := range c.Roles {
			src := SkillPath(r.Skill, c.Layout)
			if _, done := s.skills[src]; !r.Judge || done {
				continue
			}
			sk, err := copySkill(src, skillDir)
			if err != nil {
				s.Warnings = append(s.Warnings, fmt.Sprintf("judge %s: skill %s: %v", r.Name, src, err))
				continue
			}
			s.skills[src] = sk
		}
		pruneSkillCopies(skillDir, s.skills, now)
	}
	c.snapshot = s
	return s, nil
}

// copySkill copies the skill file src (EmbeddedSkill: the binary's copy) to
// dir/<hash>/SkillCopyName (kept as is when that copy already holds the
// same text).
func copySkill(src, dir string) (snapSkill, error) {
	var b []byte
	var st fileStamp
	var err error
	if src == EmbeddedSkill {
		b, st = magnum.Skill, fileStamp{embedded: true, sum: sha256.Sum256(magnum.Skill)}
	} else if b, st, err = readStamped(src); err != nil {
		return snapSkill{}, err
	}
	sum := hex.EncodeToString(st.sum[:])[:12]
	dst := filepath.Join(dir, sum, SkillCopyName)
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, b) {
		return snapSkill{copy: dst, stamp: st}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return snapSkill{}, err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return snapSkill{}, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return snapSkill{}, err
	}
	return snapSkill{copy: dst, stamp: st}, nil
}

// pruneSkillCopies removes the copies under dir that no current judge uses
// and that are older than skillCopyKeep; best effort.
func pruneSkillCopies(dir string, keep map[string]snapSkill, now time.Time) {
	used := map[string]bool{}
	for _, sk := range keep {
		used[filepath.Base(filepath.Dir(sk.copy))] = true
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() || used[e.Name()] || !skillCopyDirRe.MatchString(e.Name()) {
			continue
		}
		fi, err := os.Stat(filepath.Join(dir, e.Name(), SkillCopyName))
		if err == nil && now.Sub(fi.ModTime()) < skillCopyKeep {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// PromptSnapshot returns the snapshot SnapshotPrompts installed; nil when
// prompts are read from disk (every CLI command).
func (c *Config) PromptSnapshot() *PromptSnapshot { return c.snapshot }

// SkillFile is the file a judge prompt names for the skill at src
// (SkillPath): its startup copy when the daemon's snapshot holds one, else
// src itself.
func (c *Config) SkillFile(src string) string {
	if s := c.snapshot; s != nil {
		if sk, ok := s.skills[src]; ok {
			return sk.copy
		}
	}
	return src
}

// prompt returns the snapshot's prompt name; ok is false on a nil snapshot
// or a name it does not hold.
func (s *PromptSnapshot) prompt(name string) (Prompt, bool) {
	if s == nil {
		return Prompt{}, false
	}
	sp, ok := s.prompts[name]
	return sp.p, ok
}

// Files counts the files the snapshot holds: prompts (embedded defaults
// included) and skill copies.
func (s *PromptSnapshot) Files() int {
	if s == nil {
		return 0
	}
	return len(s.prompts) + len(s.skills)
}

// Changed lists what differs on disk from the snapshot, sorted: the names
// of prompts whose file changed or disappeared, or (an embedded default)
// that a file in prompts_dir would now override, and the paths of skills
// whose file changed or disappeared. The next restart loads them.
func (s *PromptSnapshot) Changed() []string {
	if s == nil {
		return nil
	}
	var out []string
	for name, sp := range s.prompts {
		if !sp.p.Embedded {
			if !sp.stamp.same(sp.p.Path) {
				out = append(out, name)
			}
			continue
		}
		if s.dir != "" && filepath.IsAbs(s.dir) {
			if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
				out = append(out, name)
			}
		}
	}
	for src, sk := range s.skills {
		if !sk.stamp.same(src) {
			out = append(out, src)
		}
	}
	slices.Sort(out)
	return out
}

package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/paths"
)

// Staging is the [staging] table: where magnum keeps the treeless mirrors
// that tell which open PRs are live on a staging environment
// (internal/staging). The environments themselves are the
// [[factory.repo.staging]] entries of a repository (Stagings).
type Staging struct {
	// MirrorDir holds one treeless bare mirror per repository,
	// <mirror_dir>/<owner>/<repo>.git; "" = <state dir>/staging (Load fills
	// it in). Absolute after ~ expands. A path under /Volumes whose volume
	// is not mounted leaves every staging status unknown, and nothing is
	// created there.
	MirrorDir string `toml:"mirror_dir"`
}

// StagingBranchPattern is what a [[factory.repo.staging]] branch must match:
// a deploy (R46) pushes only to a staging-<name> branch.
var StagingBranchPattern = regexp.MustCompile(`^staging-[a-z0-9-]+$`)

// Stagings returns the staging environments of the [[factory.repo]] for
// owner/name (case ignored), in the order the block lists them: a copy, nil
// when no block or no entry names one.
func (c *Config) Stagings(repo string) []FactoryStaging {
	r, ok := c.FactoryRepo(repo)
	if !ok {
		return nil
	}
	return slices.Clone(r.Staging)
}

// neverStaging reports whether branch is one a staging entry may never name,
// whatever StagingBranchPattern says: master, main, the repository's base,
// or anything that starts with production (case ignored).
func neverStaging(branch, base string) bool {
	return branch == "master" || branch == "main" || branch == base ||
		strings.HasPrefix(strings.ToLower(branch), "production")
}

// expandStaging resolves ~ in [staging] mirror_dir and fills its default,
// <state dir>/staging, when the layout names one.
func (c *Config) expandStaging() {
	c.Staging.MirrorDir = paths.Expand(c.Staging.MirrorDir)
	if c.Staging.MirrorDir == "" && c.Layout.Valid() {
		c.Staging.MirrorDir = filepath.Join(c.Layout.State(), "staging")
	}
}

// validateStaging checks [staging]: mirror_dir is absolute after ~ expands
// (or empty, without a layout), and neither /Volumes nor a volume's root
// (/Volumes/<name>), the directories a volume mounts on, where a mirror
// would be written on the system disk while the volume is away.
func (c *Config) validateStaging() []error {
	d := c.Staging.MirrorDir
	switch clean := filepath.Clean(d); {
	case d == "":
	case !filepath.IsAbs(d):
		return []error{fmt.Errorf("staging.mirror_dir must be absolute after ~ expands, got %q", d)}
	case clean == "/Volumes" || filepath.Dir(clean) == "/Volumes":
		return []error{fmt.Errorf("staging.mirror_dir %q must be a directory on a volume, not /Volumes or a volume's root (/Volumes/<name>/magnum-staging)", d)}
	}
	return nil
}

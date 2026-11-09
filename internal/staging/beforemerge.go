package staging

import (
	"slices"

	"github.com/zhuravel/magnum/internal/textx"
)

// DeployBeforeMerge reports whether a PR whose changed files are paths must
// go to a staging before its merge: it changes a migration (migrations are
// every such path, in byte order, each once; nil for none, even when
// needed is false), and every environment in envs was read and holds none
// of the PR's head. An environment that is latest or pushed holds the head,
// and one that is unknown may hold it, so either makes needed false: the
// needs-you line says only what the mirror backs. No environments (staging
// not configured) is never needed. The board and the factory's card share
// this rule and its words (DeployBeforeMergeWords).
func DeployBeforeMerge(paths []string, envs []EnvStatus) (migrations []string, needed bool) {
	for _, p := range paths {
		if isMigration(p) {
			migrations = append(migrations, p)
		}
	}
	slices.Sort(migrations)
	migrations = slices.Compact(migrations)
	if len(migrations) == 0 || len(envs) == 0 {
		return migrations, false
	}
	return migrations, !slices.ContainsFunc(envs, func(e EnvStatus) bool {
		return e.State == StateLatest || e.State == StatePushed || e.State == StateUnknown
	})
}

// DeployBeforeMergeWords is the needs-you line of a PR that DeployBeforeMerge
// says needs a staging deploy before its merge: the first of its migrations
// (byte order) by name, and how many more there are. The first clause,
// "deploy to a staging before the merge", is the same for any count: the
// board stores the words as they are, and the factory's short form is
// that clause (it cuts at the first " ("). No migration is that clause
// alone.
func DeployBeforeMergeWords(migrations []string) string {
	const first = "deploy to a staging before the merge"
	if len(migrations) == 0 {
		return first
	}
	words := first + " (it changes " + migrations[0]
	if more := len(migrations) - 1; more > 0 {
		words += " and " + textx.Count(more, "more migration", "more migrations")
	}
	return words + ")"
}

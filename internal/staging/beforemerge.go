package staging

import "slices"

// DeployBeforeMerge reports whether a PR whose changed files are paths must
// go to a staging before its merge: it changes a migration (migration is the
// first such path, in byte order), and every environment in envs was read
// and holds none of the PR's head. An environment that is latest or pushed
// holds the head, and one that is unknown may hold it, so either makes needed
// false: the needs-you line says only what the mirror backs. No environments
// (staging not configured) is never needed. The board and the factory's card
// share this rule and its words (DeployBeforeMergeWords).
func DeployBeforeMerge(paths []string, envs []EnvStatus) (migration string, needed bool) {
	for _, p := range paths {
		if isMigration(p) && (migration == "" || p < migration) {
			migration = p
		}
	}
	if migration == "" || len(envs) == 0 {
		return migration, false
	}
	return migration, !slices.ContainsFunc(envs, func(e EnvStatus) bool {
		return e.State == StateLatest || e.State == StatePushed || e.State == StateUnknown
	})
}

// DeployBeforeMergeWords is the needs-you line of a PR that DeployBeforeMerge
// says needs a staging deploy before its merge.
func DeployBeforeMergeWords(migration string) string {
	return "deploy to a staging before the merge (it changes " + migration + ")"
}

package engine

import (
	"testing"

	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
)

// StagingFirst reads the head's own file list only, and staging's rule
// over the envs as a reader shows them: unknown holds nothing back.
func TestStagingFirstIsTheMigrationOfTheHeadsFilesThatNoStagingHolds(t *testing.T) {
	files := store.PRFiles{HeadSHA: fixHead, Paths: []string{"app/a.rb", "db/schema.rb", "db/sharded_migrate/2026_b\x1b.rb"}}
	none := []StagingEnv{{Env: "void", State: string(staging.StateNone)}, {Env: "bart", State: string(staging.StateOutdated)}}
	if got := StagingFirst(files, true, fixHead, none); got != "db/sharded_migrate/2026_b .rb" {
		t.Errorf("StagingFirst = %q, want the migration, its escape a space", got)
	}
	for name, c := range map[string]struct {
		files  store.PRFiles
		listed bool
		head   string
		shown  []StagingEnv
	}{
		"no list":         {files, false, fixHead, none},
		"another head":    {files, true, fixPushed, none},
		"no staging envs": {files, true, fixHead, nil},
		"live on one":     {files, true, fixHead, []StagingEnv{{Env: "void", State: "latest"}, {Env: "bart", State: "none"}}},
		"pushed to one":   {files, true, fixHead, []StagingEnv{{Env: "void", State: "pushed"}, {Env: "bart", State: "none"}}},
		"unknown on one":  {files, true, fixHead, []StagingEnv{{Env: "void", State: "unknown"}, {Env: "bart", State: "none"}}},
		"no migration":    {store.PRFiles{HeadSHA: fixHead, Paths: []string{"app/a.rb", "db/schema.rb", "db/shard_1_schema.rb"}}, true, fixHead, none},
	} {
		if got := StagingFirst(c.files, c.listed, c.head, c.shown); got != "" {
			t.Errorf("%s: StagingFirst = %q, want none", name, got)
		}
	}
}

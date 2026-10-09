package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
)

// countingQuerier counts the queries run through it.
type countingQuerier struct {
	querier
	queries atomic.Int32
}

func (c *countingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.queries.Add(1)
	return c.querier.QueryContext(ctx, query, args...)
}

func (c *countingQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.queries.Add(1)
	return c.querier.QueryRowContext(ctx, query, args...)
}

// `magnum notes history` lists every version with its files: they are read
// in one query for all the versions, each version getting its own files in
// path order and an empty list when it has none.
func TestNotesHistoryReadsTheFilesOfAllVersionsInOneQuery(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	var recorded []NotesVersion
	for i := range 6 {
		var files []string
		for j := range i % 3 { // 0, 1 or 2 files; one shared by every version that has any
			files = append(files, fmt.Sprintf("z%d_spec.rb", j), "x"+strconv.Itoa(j*i))
		}
		if i%3 != 0 {
			files = append(files, "run.sh", "bin/rspec\n")
		}
		v, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromHuman,
			Content: notesContent("notes "+strconv.Itoa(i)+"\n", files...)})
		if err != nil {
			t.Fatal(err)
		}
		recorded = append([]NotesVersion{v}, recorded...) // the history is newest first
		clk.Add(1)
	}
	vs, err := st.NotesHistory(ctx, repo.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range vs {
		vs[i].Files = nil
	}
	q := &countingQuerier{querier: st.db}
	if err := withFiles(ctx, q, vs); err != nil {
		t.Fatal(err)
	}
	if n := q.queries.Load(); n != 1 {
		t.Errorf("withFiles of %d versions ran %d queries, want 1", len(vs), n)
	}
	if len(vs) != len(recorded) {
		t.Fatalf("history = %d versions, want %d", len(vs), len(recorded))
	}
	for i, v := range vs {
		if v.ID != recorded[i].ID || !reflect.DeepEqual(v.Files, recorded[i].Files) || v.Files == nil {
			t.Errorf("version %d files = %+v, want %+v", v.ID, v.Files, recorded[i].Files)
		}
	}
	if err := withFiles(ctx, q, nil); err != nil || q.queries.Load() != 1 {
		t.Errorf("withFiles of no versions: %v, %d queries in all", err, q.queries.Load())
	}
}

// BenchmarkNotesHistory reads a history of 300 versions of six files each.
func BenchmarkNotesHistory(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, Repo{NodeID: "R_1", Owner: "talkable", Name: "talkable", WatchOwner: "talkable",
		DefaultBranch: "master", Mode: RepoModePool})
	if err != nil {
		b.Fatal(err)
	}
	for i := range 300 {
		var files []string
		for j := range 6 {
			files = append(files, fmt.Sprintf("f%d_spec.rb", j), fmt.Sprintf("body %d", (i/10)*10+j))
		}
		if _, _, err := st.RecordNotesVersion(ctx, NotesVersionInput{RepoID: repo.ID, Source: NotesFromHuman,
			Content: notesContent("notes "+strconv.Itoa(i)+"\n", files...)}); err != nil {
			b.Fatal(err)
		}
	}
	for b.Loop() {
		if vs, err := st.NotesHistory(ctx, repo.ID, 0); err != nil || len(vs) != 300 || len(vs[0].Files) != 6 {
			b.Fatal(len(vs), err)
		}
	}
}

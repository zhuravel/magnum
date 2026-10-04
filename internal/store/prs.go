package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// UpsertRepo inserts or refreshes a repository keyed by NodeID and returns
// the stored row. last_seen_at becomes now; first_synced_at is only ever set
// once; a nil ClonePath keeps the stored one; an empty DefaultBranch means
// "master" and an empty WatchOwner means Owner.
func (s *Store) UpsertRepo(ctx context.Context, r Repo) (Repo, error) {
	if r.NodeID == "" || r.Owner == "" || r.Name == "" || r.Mode == "" {
		return Repo{}, fmt.Errorf("upsert repo %s/%s: node_id, owner, name and mode are required", r.Owner, r.Name)
	}
	if r.DefaultBranch == "" {
		r.DefaultBranch = "master"
	}
	if r.WatchOwner == "" {
		r.WatchOwner = r.Owner
	}
	var out Repo
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO repos (node_id, owner, name, watch_owner, clone_path, default_branch, mode, first_synced_at, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(node_id) DO UPDATE SET
  owner = excluded.owner, name = excluded.name, watch_owner = excluded.watch_owner,
  clone_path = COALESCE(excluded.clone_path, repos.clone_path),
  default_branch = excluded.default_branch, mode = excluded.mode,
  first_synced_at = COALESCE(repos.first_synced_at, excluded.first_synced_at),
  last_seen_at = excluded.last_seen_at`,
			r.NodeID, r.Owner, r.Name, r.WatchOwner, r.ClonePath, r.DefaultBranch, r.Mode,
			mustDB(r.FirstSyncedAt), FormatTime(s.now()))
		if err != nil {
			return mapErr(err)
		}
		out, err = scanRepo(tx.QueryRowContext(ctx, "SELECT "+cols("", repoColumns)+" FROM repos WHERE node_id = ?", r.NodeID))
		return err
	})
	if err != nil {
		return Repo{}, fmt.Errorf("upsert repo %s/%s: %w", r.Owner, r.Name, err)
	}
	return out, nil
}

// SetRepoClonePath records where the repository's main clone lives; ""
// clears a path that turned out not to be a clone (UpsertRepo can only keep
// or replace it).
func (s *Store) SetRepoClonePath(ctx context.Context, repoID int64, path string) error {
	var v any
	if path != "" {
		v = path
	}
	res, err := s.db.ExecContext(ctx, "UPDATE repos SET clone_path = ? WHERE id = ?", v, repoID)
	if err != nil {
		return fmt.Errorf("repo %d clone path: %w", repoID, mapErr(err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("repo %d: %w", repoID, ErrNotFound)
	}
	return nil
}

// RepoByFullName looks up "owner/name" case-insensitively.
func (s *Store) RepoByFullName(ctx context.Context, fullName string) (Repo, error) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok {
		return Repo{}, fmt.Errorf("repo %q: want owner/name", fullName)
	}
	r, err := scanRepo(s.db.QueryRowContext(ctx, "SELECT "+cols("", repoColumns)+
		" FROM repos WHERE owner = ? COLLATE NOCASE AND name = ? COLLATE NOCASE", owner, name))
	if err != nil {
		return Repo{}, notFound(err, "repo", fullName)
	}
	return r, nil
}

// RepoByID looks up a repository by id.
func (s *Store) RepoByID(ctx context.Context, id int64) (Repo, error) {
	r, err := scanRepo(s.db.QueryRowContext(ctx, "SELECT "+cols("", repoColumns)+" FROM repos WHERE id = ?", id))
	if err != nil {
		return Repo{}, notFound(err, "repo", id)
	}
	return r, nil
}

// ListRepos returns every repository ordered by owner/name.
func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+cols("", repoColumns)+" FROM repos ORDER BY owner, name")
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	return collect(rows, scanRepo)
}

// GitHubPR is what the poller knows about a PR. Pointer fields (and nil
// Labels, empty GHState) mean "not fetched this time": the stored value is
// kept. Number, URL, HeadSHA and IsDraft are always present in the radar.
type GitHubPR struct {
	RepoID      int64
	NodeID      string
	Number      int
	URL         string
	HeadSHA     string
	IsDraft     bool
	Title       *string
	AuthorLogin *string
	AuthorType  *string
	HeadRef     *string
	BaseRef     *string
	IsCrossRepo *bool
	// ReviewRequested is whether the watched user is a requested reviewer
	// (nil = keep the stored value).
	ReviewRequested *bool
	Labels          []string
	GHState         string
	GHUpdatedAt     *time.Time
	MergedAt        *time.Time
	ClosedAt        *time.Time

	// Board fields from Details; nil = keep the stored value.
	Assignees          []string
	RequestedReviewers []string // teams as "team:<slug>"
	LatestReviews      []LatestReview
	BaseSHA            *string
	// DetailsAt stamps prs.details_at (the Details were fetched now). It is
	// bookkeeping: on its own it neither counts as Changed nor forces a write
	// when the stored value is already set.
	DetailsAt *time.Time
	// AuthorAssociation is the Details' authorAssociation (nil = keep).
	AuthorAssociation *string

	// InitialState and Identity are used only when the PR is new.
	InitialState string
	Identity     string
}

// PRUpsert reports what UpsertPRFromGitHub did.
type PRUpsert struct {
	PR          PR
	New         bool // inserted now
	HeadChanged bool // existing PR whose head_sha changed (head_changed_at = now)
	Changed     bool // any GitHub-derived column changed (always true when New)
}

// UpsertPRFromGitHub inserts a PR (state InitialState, head_changed_at now)
// or updates only its GitHub-derived columns. It never touches the
// automation columns (state, throttles, missing_since, …): the engine decides
// transitions from the returned flags. A no-op refresh writes nothing.
func (s *Store) UpsertPRFromGitHub(ctx context.Context, in GitHubPR) (PRUpsert, error) {
	if in.NodeID == "" || in.HeadSHA == "" || in.URL == "" || in.RepoID == 0 || in.Number <= 0 {
		return PRUpsert{}, fmt.Errorf("upsert pr #%d: repo_id, node_id, number, url and head_sha are required", in.Number)
	}
	var res PRUpsert
	err := s.tx(ctx, func(tx *sql.Tx) error {
		cur, err := scanPR(tx.QueryRowContext(ctx, "SELECT "+cols("", prColumns)+" FROM prs WHERE node_id = ?", in.NodeID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id, err := s.insertPR(ctx, tx, in)
			if err != nil {
				return err
			}
			res.New, res.Changed = true, true
			res.PR, err = scanPR(tx.QueryRowContext(ctx, "SELECT "+cols("", prColumns)+" FROM prs WHERE id = ?", id))
			return err
		case err != nil:
			return err
		}

		u := &PRUpdate{Update{table: prTable}}
		setIf := func(col string, changed bool, v any) {
			if changed {
				u.Set(col, v)
			}
		}
		now := s.now()
		if in.HeadSHA != cur.HeadSHA {
			res.HeadChanged = true
			u.Set("head_sha", in.HeadSHA)
			u.Set("head_changed_at", now)
		}
		setIf("number", in.Number != cur.Number, in.Number)
		setIf("url", in.URL != cur.URL, in.URL)
		setIf("is_draft", in.IsDraft != cur.IsDraft, in.IsDraft)
		setIf("title", in.Title != nil && *in.Title != Deref(cur.Title), in.Title)
		setIf("author_login", in.AuthorLogin != nil && *in.AuthorLogin != Deref(cur.AuthorLogin), in.AuthorLogin)
		setIf("author_type", in.AuthorType != nil && *in.AuthorType != Deref(cur.AuthorType), in.AuthorType)
		setIf("head_ref", in.HeadRef != nil && *in.HeadRef != Deref(cur.HeadRef), in.HeadRef)
		setIf("base_ref", in.BaseRef != nil && *in.BaseRef != Deref(cur.BaseRef), in.BaseRef)
		setIf("is_cross_repo", in.IsCrossRepo != nil && *in.IsCrossRepo != cur.IsCrossRepo, in.IsCrossRepo)
		setIf("review_requested", in.ReviewRequested != nil && *in.ReviewRequested != cur.ReviewRequested, in.ReviewRequested)
		setIf("labels_json", in.Labels != nil && !slices.Equal(in.Labels, cur.Labels), in.Labels)
		setIf("gh_state", in.GHState != "" && in.GHState != cur.GHState, in.GHState)
		setIf("gh_updated_at", timeChanged(in.GHUpdatedAt, cur.GHUpdatedAt), in.GHUpdatedAt)
		setIf("merged_at", timeChanged(in.MergedAt, cur.MergedAt), in.MergedAt)
		setIf("closed_at", timeChanged(in.ClosedAt, cur.ClosedAt), in.ClosedAt)
		setIf("assignees_json", in.Assignees != nil && !slices.Equal(in.Assignees, cur.Assignees), in.Assignees)
		setIf("requested_reviewers_json", in.RequestedReviewers != nil && !slices.Equal(in.RequestedReviewers, cur.RequestedReviewers), in.RequestedReviewers)
		setIf("latest_reviews_json", in.LatestReviews != nil && !slices.EqualFunc(in.LatestReviews, cur.LatestReviews, LatestReview.equal), in.LatestReviews)
		setIf("base_sha", in.BaseSHA != nil && *in.BaseSHA != Deref(cur.BaseSHA), in.BaseSHA)
		// A PR nothing reviewed yet follows its watch's identity here, while
		// idle: a forced round keeps what `magnum review --as` chose and a PR
		// with a round in flight keeps the identity its sessions post as. A
		// reviewed PR migrates when its next round is dispatched (the engine
		// records the former identity, whose reviews stay the PR's history).
		setIf("identity", in.Identity != "" && in.Identity != cur.Identity && Deref(cur.ReviewedSHA) == "" && !cur.Forced &&
			slices.Contains([]string{PRBaseline, PRIneligible, PRQueued}, cur.State), in.Identity)
		setIf("author_association", in.AuthorAssociation != nil &&
			(cur.AuthorAssociation == nil || *in.AuthorAssociation != *cur.AuthorAssociation), in.AuthorAssociation)
		changed := len(u.sets) > 0
		setIf("details_at", in.DetailsAt != nil && (changed || cur.DetailsAt == nil), in.DetailsAt)
		if len(u.sets) == 0 && u.err == nil {
			res.PR = cur
			return nil
		}
		res.Changed = changed
		if err := s.apply(ctx, tx, prTable, cur.ID, nil, "", &u.Update); err != nil {
			return err
		}
		res.PR, err = scanPR(tx.QueryRowContext(ctx, "SELECT "+cols("", prColumns)+" FROM prs WHERE id = ?", cur.ID))
		return err
	})
	if err != nil {
		return PRUpsert{}, fmt.Errorf("upsert pr %s #%d: %w", in.NodeID, in.Number, err)
	}
	return res, nil
}

func (s *Store) insertPR(ctx context.Context, tx *sql.Tx, in GitHubPR) (int64, error) {
	if in.InitialState == "" || in.Identity == "" {
		return 0, errors.New("new PR needs InitialState and Identity")
	}
	labels := in.Labels
	if labels == nil {
		labels = []string{}
	}
	assignees, requested := in.Assignees, in.RequestedReviewers
	if assignees == nil {
		assignees = []string{}
	}
	if requested == nil {
		requested = []string{}
	}
	ghState := in.GHState
	if ghState == "" {
		ghState = GHOpen
	}
	now := FormatTime(s.now())
	res, err := tx.ExecContext(ctx, `
INSERT INTO prs (repo_id, node_id, number, url, title, author_login, author_type, head_ref, base_ref,
  head_sha, head_changed_at, is_draft, is_cross_repo, review_requested, labels_json, gh_state, gh_updated_at,
  merged_at, closed_at, state, identity, created_at, updated_at,
  assignees_json, requested_reviewers_json, latest_reviews_json, base_sha, details_at, author_association)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.RepoID, in.NodeID, in.Number, in.URL, in.Title, in.AuthorLogin, in.AuthorType, in.HeadRef, in.BaseRef,
		in.HeadSHA, now, boolInt(in.IsDraft), boolInt(Deref(in.IsCrossRepo)),
		boolInt(Deref(in.ReviewRequested)), mustDB(labels), ghState,
		mustDB(in.GHUpdatedAt), mustDB(in.MergedAt), mustDB(in.ClosedAt), in.InitialState, in.Identity, now, now,
		mustDB(assignees), mustDB(requested), mustDB(in.LatestReviews), in.BaseSHA, mustDB(in.DetailsAt), in.AuthorAssociation)
	if err != nil {
		return 0, mapErr(err)
	}
	return res.LastInsertId()
}

func timeChanged(in, cur *time.Time) bool {
	return in != nil && (cur == nil || !in.Equal(*cur))
}

// mustDB converts values whose types dbValue always accepts.
func mustDB(v any) any {
	dv, err := dbValue(v)
	if err != nil {
		panic(err)
	}
	return dv
}

// PRByID looks up a PR by id.
func (s *Store) PRByID(ctx context.Context, id int64) (PR, error) {
	p, err := scanPR(s.db.QueryRowContext(ctx, "SELECT "+cols("", prColumns)+" FROM prs WHERE id = ?", id))
	if err != nil {
		return PR{}, notFound(err, "pr", id)
	}
	return p, nil
}

// PRByNodeID looks up a PR by its GitHub node id.
func (s *Store) PRByNodeID(ctx context.Context, nodeID string) (PR, error) {
	p, err := scanPR(s.db.QueryRowContext(ctx, "SELECT "+cols("", prColumns)+" FROM prs WHERE node_id = ?", nodeID))
	if err != nil {
		return PR{}, notFound(err, "pr", nodeID)
	}
	return p, nil
}

// PRByRepoNumber looks up PR #number of repository repoID.
func (s *Store) PRByRepoNumber(ctx context.Context, repoID int64, number int) (PR, error) {
	p, err := scanPR(s.db.QueryRowContext(ctx, "SELECT "+cols("", prColumns)+
		" FROM prs WHERE repo_id = ? AND number = ?", repoID, number))
	if err != nil {
		return PR{}, notFound(err, "pr", fmt.Sprintf("repo %d #%d", repoID, number))
	}
	return p, nil
}

// PRFilter selects PRs for ListPRs. Zero values match everything.
type PRFilter struct {
	RepoID int64
	States []string
	Limit  int
}

// ListPRs returns PRs matching f ordered by repo and number.
func (s *Store) ListPRs(ctx context.Context, f PRFilter) ([]PR, error) {
	var where []string
	var args []any
	if f.RepoID != 0 {
		where = append(where, "repo_id = ?")
		args = append(args, f.RepoID)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+placeholders(len(f.States))+")")
		args = append(args, anys(f.States)...)
	}
	q := "SELECT " + cols("", prColumns) + " FROM prs"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY repo_id, number"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list prs: %w", err)
	}
	return collect(rows, scanPR)
}

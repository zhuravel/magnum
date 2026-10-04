package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// RefParser resolves the PR references users type (URL, owner/repo#N,
// repo#N, #N, N) against a default repository ("owner/name").
type RefParser struct{ DefaultRepo string }

// ResolvePR parses ref with github.ParseRef and the parser's default repo.
func (p RefParser) ResolvePR(_ context.Context, ref string) (owner, repo string, number int, err error) {
	owner, repo, number, err = github.ParseRef(ref, p.DefaultRepo)
	if err != nil {
		return "", "", 0, fmt.Errorf("pr ref %q: %w", ref, err)
	}
	return owner, repo, number, nil
}

// Refs is the App's RefParser (cfg.Daemon.DefaultRepo).
func (a *App) Refs() RefParser { return RefParser{DefaultRepo: a.Config.Daemon.DefaultRepo} }

// ResolvePR parses a PR reference with cfg.Daemon.DefaultRepo.
func (a *App) ResolvePR(ctx context.Context, ref string) (owner, repo string, number int, err error) {
	return a.Refs().ResolvePR(ctx, ref)
}

// LookupPR resolves ref and loads the repository and PR rows. A repository
// or PR the registry does not know wraps store.ErrNotFound.
func (a *App) LookupPR(ctx context.Context, ref string) (store.Repo, store.PR, error) {
	return LookupPR(ctx, a.Store, a.Refs(), ref)
}

// LookupPR resolves ref with p and loads its rows from st.
func LookupPR(ctx context.Context, st *store.Store, p RefParser, ref string) (store.Repo, store.PR, error) {
	owner, name, number, err := p.ResolvePR(ctx, ref)
	if err != nil {
		return store.Repo{}, store.PR{}, err
	}
	repo, err := st.RepoByFullName(ctx, owner+"/"+name)
	if errors.Is(err, store.ErrNotFound) && isShortRepoRef(ref) {
		// repo#N took its owner from the default repository; a repository of
		// that name under another watched owner is what the user meant. A
		// bare number (N or #N) names the default repository itself, so it
		// never falls back to a same-named repository of another owner.
		repo, err = repoByName(ctx, st, name, owner)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Repo{}, store.PR{}, fmt.Errorf("repository %s/%s is not in the registry yet: %w", owner, name, err)
		}
		return store.Repo{}, store.PR{}, err
	}
	pr, err := st.PRByRepoNumber(ctx, repo.ID, number)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return repo, store.PR{}, fmt.Errorf("%s#%d is not in the registry (open PRs appear after the next poll): %w", repo.FullName(), number, err)
		}
		return repo, store.PR{}, err
	}
	return repo, pr, nil
}

// isShortRepoRef reports whether ref has the form repo#N: a repository name
// before the '#' and no owner or URL path (no '/').
func isShortRepoRef(ref string) bool {
	repo, _, ok := strings.Cut(strings.TrimSpace(ref), "#")
	return ok && repo != "" && !strings.Contains(ref, "/")
}

// repoByName finds the registered repository called name under any owner;
// several matches are an error naming them (use owner/name), none wraps
// store.ErrNotFound for defaultOwner/name.
func repoByName(ctx context.Context, st *store.Store, name, defaultOwner string) (store.Repo, error) {
	repos, err := st.ListRepos(ctx)
	if err != nil {
		return store.Repo{}, err
	}
	var found []store.Repo
	for _, r := range repos {
		if strings.EqualFold(r.Name, name) {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return store.Repo{}, fmt.Errorf("repo %s/%s: %w", defaultOwner, name, store.ErrNotFound)
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, r := range found {
		names[i] = r.FullName()
	}
	return store.Repo{}, fmt.Errorf("%s#N is ambiguous: %s (use owner/name#N)", name, strings.Join(names, ", "))
}

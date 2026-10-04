package engine

// Identity migration: a PR follows its watch's posting identity. When the
// configuration moves a watch to another identity (an App replaced by
// another App, a user by an App), the next round of each of its PRs posts as
// the new identity, parks the sessions the old one's gh config ran in (the
// identity-change path of startSessions) and treats the reviews and threads
// the old login posted as its own history: the previous review, the reply
// contract's threads, the judge's earlier findings. Once its review is
// posted, what the old identity left standing (a change request, an App's
// approval) is dismissed with the old identity's own credentials. A PR
// pinned with `magnum review --as` keeps its identity.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// KVPRFormerIdentities holds the identities a PR posted as before it
// migrated to its current one (a JSON list of identity names, newest last).
func KVPRFormerIdentities(prID int64) string { return fmt.Sprintf("pr.%d.former_identities", prID) }

// KVPRIdentityPinned holds the identity `magnum review --as` pinned the PR
// to; a pinned PR never migrates.
func KVPRIdentityPinned(prID int64) string { return fmt.Sprintf("pr.%d.identity_pinned", prID) }

// FormerDismissMessage is the reason a dismissed review of a former
// identity shows on GitHub; the arguments are the new review's commit
// (short) and the login it was posted as.
const FormerDismissMessage = "magnum: superseded by the review of %s posted as %s (this PR's reviewer changed)"

// formerIdentities reads KVPRFormerIdentities (nil when absent or
// unreadable).
func (e *Engine) formerIdentities(ctx context.Context, prID int64) []string {
	v, ok := e.getKV(ctx, KVPRFormerIdentities(prID))
	if !ok || v == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(v), &out) != nil {
		return nil
	}
	return out
}

// formerLogins are the logins of pr's former identities (their configured
// login, REST form), without its current login; identities no longer
// configured are left out.
func (e *Engine) formerLogins(ctx context.Context, pr store.PR) []string {
	cur := e.reviewerLogin(pr.Identity)
	var out []string
	for _, name := range e.formerIdentities(ctx, pr.ID) {
		id := e.cfg.IdentityByName(name)
		if id == nil || id.Login == "" || sameAccount(id.Login, cur) || slices.Contains(out, id.Login) {
			continue
		}
		out = append(out, id.Login)
	}
	return out
}

// sameAccount reports whether two configured logins (REST form) name the
// same account: the same login and both or neither an App's ("[bot]").
// github.SameLogin alone would take the user "zhuravel" for the App
// "zhuravel[bot]".
func sameAccount(a, b string) bool {
	bot := func(l string) bool { return strings.HasSuffix(strings.ToLower(strings.TrimSpace(l)), "[bot]") }
	return github.SameLogin(a, b) && bot(a) == bot(b)
}

// migrateIdentity moves a PR about to be dispatched from an identity its
// watch no longer posts as to the watch's (in the registry and in pr):
// the old one joins KVPRFormerIdentities and pr.identity_migrated is
// recorded. A PR pinned with `magnum review --as` and a paused round
// (its judge's turn continues as the identity it started with) are left
// alone.
func (e *Engine) migrateIdentity(ctx context.Context, pr *store.PR, repo store.Repo, w *config.Watch) {
	if w == nil || w.Identity == "" || pr.Identity == w.Identity || pr.State == store.PRPaused {
		return
	}
	if pin, _ := e.getKV(ctx, KVPRIdentityPinned(pr.ID)); pin != "" {
		return
	}
	if e.cfg.IdentityByName(w.Identity) == nil {
		return
	}
	old := pr.Identity
	err := e.st.UpdatePR(ctx, pr.ID, func(u *store.PRUpdate) {
		u.Where("identity", old)
		u.Set("identity", w.Identity)
	})
	if err != nil {
		if !errors.Is(err, store.ErrConflict) {
			e.log.Warn("identity migration", "pr", pr.ID, "err", err)
		}
		return
	}
	former := slices.DeleteFunc(e.formerIdentities(ctx, pr.ID), func(n string) bool { return n == old || n == w.Identity })
	former = append(former, old)
	if b, err := json.Marshal(former); err == nil {
		e.setKV(ctx, KVPRFormerIdentities(pr.ID), string(b))
	}
	pr.Identity = w.Identity
	oldLogin, newLogin := e.reviewerLogin(old), e.reviewerLogin(w.Identity)
	e.event(ctx, "info", prSubject(repo, pr.Number), "pr.identity_migrated",
		fmt.Sprintf("identity %s → %s: the watch posts as %s now; the next round parks the old sessions and treats the reviews and threads of %s as its own",
			old, w.Identity, newLogin, oldLogin),
		map[string]any{"from": old, "to": w.Identity, "former_identities": former})
}

// dismissFormer withdraws, once a review by the PR's current identity is
// posted, what its former identities left standing on the PR: each one's
// own CHANGES_REQUESTED reviews (when it dismisses its stale change
// requests, dismiss_own_stale_change_requests) and an App's approvals
// (unless keep_approvals), each dismissed with that identity's own
// credentials. Other logins' reviews are never touched. Failures are
// warnings: the new review stands either way.
func (e *Engine) dismissFormer(ctx context.Context, job *roundJob, pr store.PR, target string, res pipeline.RoundResult) {
	if e.d.DryRun || res.ReviewID == 0 || e.d.GitHub == nil {
		return
	}
	cur := e.reviewerLogin(pr.Identity)
	subject := prSubject(job.repo, pr.Number)
	full := job.repo.FullName()
	for _, name := range e.formerIdentities(ctx, pr.ID) {
		id := e.cfg.IdentityByName(name)
		if id == nil || id.Login == "" || sameAccount(id.Login, cur) {
			continue
		}
		app := id.Kind == "app"
		staleCR, approvals := id.DismissStale(), app && !e.cfg.KeepApprovals(full)
		if !staleCR && !approvals {
			continue
		}
		gh := e.d.GitHub(id.Name)
		if gh == nil {
			e.event(ctx, "warn", subject, "review.former_dismiss_failed",
				fmt.Sprintf("no GitHub client for the former identity %s: its reviews stay as they are", id.Name), nil)
			continue
		}
		reviews, err := gh.ReviewsWithMarker(ctx, job.repo.Owner, job.repo.Name, pr.Number, "")
		if err != nil {
			e.event(ctx, "warn", subject, "review.former_dismiss_failed",
				fmt.Sprintf("could not list the reviews of the former identity %s: %v", id.Name, err), nil)
			continue
		}
		for _, rv := range reviews {
			if !github.SameLogin(rv.AuthorLogin, id.Login) || github.IsBot(rv.AuthorType, rv.AuthorLogin) != app {
				continue
			}
			state := strings.ToUpper(strings.TrimSpace(rv.State))
			if !(state == "CHANGES_REQUESTED" && staleCR) && !(state == "APPROVED" && approvals) {
				continue
			}
			data := map[string]any{"review_id": rv.DatabaseID, "state": state, "identity": id.Name, "superseded_by": res.ReviewID}
			err := gh.DismissReview(ctx, job.repo.Owner, job.repo.Name, pr.Number, rv.DatabaseID, fmt.Sprintf(FormerDismissMessage, short(target), cur))
			if err != nil {
				e.event(ctx, "warn", subject, "review.former_dismiss_failed",
					fmt.Sprintf("could not dismiss %s review %d by %s: %v", state, rv.DatabaseID, id.Login, err), data)
				continue
			}
			e.event(ctx, "info", subject, "review.former_dismissed",
				fmt.Sprintf("dismissed %s review %d by the former identity %s (superseded by review %d as %s)", state, rv.DatabaseID, id.Login, res.ReviewID, cur), data)
		}
	}
}

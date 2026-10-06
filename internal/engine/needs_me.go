package engine

// PRs magnum approved that still need the operator (store.NeedsMe): GitHub
// does not count a GitHub App's approval toward a branch's required
// approvals, so the operator's approval is the one that counts, and their
// own earlier changes request blocks the PR until they approve or dismiss
// it. The daemon toasts each such PR once per head.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// needsMeToastWindow dedupes the toast of one PR and head across daemon
// restarts: a head is announced once however long it waits.
const needsMeToastWindow = 30 * 24 * time.Hour

// kindNeedsMe counts the PRs that need the operator in a batch summary.
var kindNeedsMe = notify.Kind{One: "PR needs your approval", Many: "PRs need your approval"}

// reviewGate is the registry's form of the Details' review gate: the
// opinions' logins in Account form (an App's keeps "[bot]"); nil when GitHub
// returned none.
func reviewGate(g *github.ReviewGate) *store.ReviewGate {
	if g == nil {
		return nil
	}
	out := &store.ReviewGate{Decision: g.Decision, Complete: g.Complete, Opinions: make([]store.LatestReview, 0, len(g.Opinions))}
	for _, o := range g.Opinions {
		out.Opinions = append(out.Opinions, store.LatestReview{Login: github.Account(o.AuthorLogin, o.AuthorType), State: o.State, CommitSHA: o.CommitOid})
	}
	return out
}

// noteNeedsMe toasts every PR that entered the needs-me state, once per PR
// and head (the registry's dedupe outlives a restart), with its link:
// several in one tick come as one summary. e.needsMe keeps the keys offered
// in this run, so a PR that keeps waiting is not offered to the batcher
// every tick; a PR that left the state is forgotten (a return on the same
// head stays deduped by the registry).
func (e *Engine) noteNeedsMe(ctx context.Context) {
	list, err := e.st.NeedsMePRs(ctx, e.cfg.CommentsWhenClean, e.cfg.SelfMatch())
	if err != nil {
		e.log.Warn("prs that need you", "err", err)
		return
	}
	if e.needsMe == nil {
		e.needsMe = map[string]bool{}
	}
	seen := make(map[string]bool, len(list))
	for _, n := range list {
		key := fmt.Sprintf("needs-me:%d:%s", n.PR.ID, n.PR.HeadSHA)
		seen[key] = true
		if e.needsMe[key] {
			continue
		}
		e.needsMe[key] = true
		_, name, _ := strings.Cut(n.Repo, "/")
		label := fmt.Sprintf("%s#%d", name, n.PR.Number)
		head := textx.ShortSHA(n.PR.HeadSHA)
		what, why := label+" needs your approval", "GitHub still requires an approval that counts (an App's does not)"
		if n.NeedsMe == store.NeedsMeLift {
			what, why = label+": lift your changes request", "your changes request is the only one blocking it"
		}
		e.info(notify.Item{Key: key, Title: "magnum: " + what,
			Body: fmt.Sprintf("%s\nmagnum approved %s; %s.", n.PR.URL, head, why),
			Line: what + " " + n.PR.URL, Kind: kindNeedsMe, Window: needsMeToastWindow})
	}
	for k := range e.needsMe {
		if !seen[k] {
			delete(e.needsMe, k)
		}
	}
}

package factory

// The factory's PR requests (DECISIONS "The factory becomes the only
// fixer"): what `magnum fix` and the screens' f, F, c, P and K asked of
// magnum's daemon, asked of the factory once it fixes your PRs, and the
// hand-off of a PR to a reviewer. Each kind has its payload, decoded with
// unknown fields refused as every request's is, and its checks (prChecks).
// A fix, a CI fix, a merge of the parent and a hand-off answer a dry run
// with their plan (FixPlan: what the y/N asks about, as JSON on the
// answer's first line, then the plan in words). The y of a CI fix, a merge
// of the parent or a hand-off that sends the plan back binds the request
// to it (FixAsked), as magnum's MergeAsked binds a merge of the parent: a
// binding that names no plan of its kind, one asked fixAskedWait or longer
// ago, and one whose head, base, tip, conflicts, held merge, failed checks
// or assignee moved since is refused, and nothing is done (askedRefusal).
// A hand-off takes no y without its plan; a CI fix and a merge of the
// parent take a y with none (`--yes`). A fix's y binds nothing (FixPayload
// carries no plan), as a plain `magnum fix` binds nothing on magnum's
// engine: nothing checks that the PR is still as its plan read it, which
// only tells you what the fix would do. The work behind each kind is the
// fix lane's (PRWorker, Deps.PRWork). While [factory] fix is off, every
// one of them is answered that the factory does not fix PRs yet, naming
// the key, and the worker is never asked.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/textx"
)

// The factory's PR request kinds (FixKinds).
const (
	// ReqFix starts a fix loop on your PR (FixPayload).
	ReqFix = "fix"
	// ReqFixOff takes your PR out of the factory's fixes, ReqFixOn puts it
	// back (FixSwitchPayload).
	ReqFixOff = "fix_off"
	ReqFixOn  = "fix_on"
	// ReqFixDone says you did what a fix loop left you (FixDonePayload).
	ReqFixDone = "fix_done"
	// ReqFixDecide posts your replies in the threads the fixer left you,
	// then starts the loop that builds them (FixDecidePayload).
	ReqFixDecide = "fix_decide"
	// ReqFixCI fixes your PR's failed required CI (FixCIPayload).
	ReqFixCI = "fix_ci"
	// ReqMergeParent merges your PR's parent into it (MergeParentPayload).
	ReqMergeParent = "merge_parent"
	// ReqFixStop stops the fix loop on your PR (FixStopPayload).
	ReqFixStop = "fix_stop"
	// ReqHandoff hands your PR to its reviewer (HandoffPayload).
	ReqHandoff = "handoff"
)

// FixKinds are the factory's PR request kinds.
var FixKinds = []string{ReqFix, ReqFixOff, ReqFixOn, ReqFixDone, ReqFixDecide, ReqFixCI, ReqMergeParent, ReqFixStop, ReqHandoff}

// FixPayload is a ReqFix: start a fix loop on the PR (PRRef), or answer
// what it would do (DryRun). Its y is bound to no plan: it carries none,
// and nothing checks the PR against one. Surface is where you asked (the
// screen, the CLI).
type FixPayload struct {
	PRRef
	DryRun  bool   `json:",omitempty"`
	Surface string `json:",omitempty"`
}

// FixSwitchPayload is a ReqFixOff or a ReqFixOn: take the PR out of the
// factory's fixes, or put it back. Neither starts a loop.
type FixSwitchPayload struct {
	PRRef
	Surface string `json:",omitempty"`
}

// FixDonePayload is a ReqFixDone: you did what a fix loop left you on the
// PR, and Seen is the digest of what the screen showed you it left (one
// that changed since is refused, by the worker).
type FixDonePayload struct {
	PRRef
	Seen    string
	Surface string `json:",omitempty"`
}

// FixDecidePayload is a ReqFixDecide: your replies (Decide) in the threads
// the fixer left you on the PR, posted as you, then the loop that builds
// them. Your words: the payload goes once the request is answered.
type FixDecidePayload struct {
	PRRef
	Decide  []FixDecision
	Surface string `json:",omitempty"`
}

// FixDecision is one reply of yours: the thread (its first comment's id)
// and what you reply there.
type FixDecision struct {
	Thread int64
	Reply  string
}

// FixCIPayload is a ReqFixCI: fix the PR's failed required CI, answer its
// plan (DryRun), or fix it bound to the plan its y/N asked about (Asked;
// nil: nothing was asked, as `--yes`).
type FixCIPayload struct {
	PRRef
	DryRun  bool      `json:",omitempty"`
	Asked   *FixAsked `json:",omitempty"`
	Surface string    `json:",omitempty"`
}

// MergeParentPayload is a ReqMergeParent: merge the PR's parent into it (a
// merge the factory holds for your y is pushed: Asked.Merge names it),
// answer its plan (DryRun), or merge bound to the plan its y/N asked about
// (Asked; nil: nothing was asked, as `--yes`, which never pushes a held
// merge).
type MergeParentPayload struct {
	PRRef
	DryRun  bool      `json:",omitempty"`
	Asked   *FixAsked `json:",omitempty"`
	Surface string    `json:",omitempty"`
}

// FixStopPayload is a ReqFixStop: stop the fix loop on the PR.
type FixStopPayload struct {
	PRRef
	Surface string `json:",omitempty"`
}

// HandoffPayload is a ReqHandoff: hand the PR to its reviewer, answer the
// brief and who it goes to (DryRun), or assign it bound to what its y/N
// asked about (Asked, which a hand-off needs: it assigns a person).
type HandoffPayload struct {
	PRRef
	DryRun  bool      `json:",omitempty"`
	Asked   *FixAsked `json:",omitempty"`
	Surface string    `json:",omitempty"`
}

// FixAsked is what the y/N of a PR request's plan asked about, which the
// request it confirms is bound to: the PR's head and when the factory
// planned it, and by kind the parent (Base: the PR's base branch), the tip
// of it the plan read, how many files its merge conflicts in and the merge
// the factory holds for your y (Merge) of a merge of the parent, the failed
// required entries and the count of the CI fix (Count: the n of "CI fix n
// of 3") of a CI fix, and the login the PR goes to (Assignee) of a
// hand-off.
type FixAsked struct {
	Head      string    `json:"head"`
	Base      string    `json:"base,omitempty"`
	Tip       string    `json:"tip,omitempty"`
	Conflicts int       `json:"conflicts,omitempty"`
	Merge     string    `json:"merge,omitempty"`
	Entries   []string  `json:"entries,omitempty"`
	Count     int       `json:"count,omitempty"`
	Assignee  string    `json:"assignee,omitempty"`
	At        time.Time `json:"at"`
}

// FixPlan is the factory's answer to a dry run of a PR request: the PR
// (owner/name#N), the request's kind, what its y/N asks about and the plan
// in words (Text, the answer's last line).
type FixPlan struct {
	Ref   string   `json:"ref"`
	Kind  string   `json:"kind"`
	Asked FixAsked `json:"asked"`
	Text  string   `json:"text"`
}

// ParseFixPlan reads the answer to a dry run of a PR request (its first
// line); false when it is none (a refusal's words).
func ParseFixPlan(result string) (FixPlan, bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(result), "\n")
	var p FixPlan
	if json.Unmarshal([]byte(line), &p) != nil || p.Ref == "" || p.Kind == "" || p.Asked.Head == "" {
		return FixPlan{}, false
	}
	return p, true
}

// result is the dry run's answer: p as JSON, then its words.
func (p FixPlan) result() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return string(b) + "\n" + p.Text, nil
}

// PRWorker is the work behind the factory's PR requests, which the fix lane
// fills. The request's checks, [factory] fix and a bound y's check ran
// before any call. Plan answers what a request of kind (ReqFix, ReqFixCI,
// ReqMergeParent or ReqHandoff) would do on pr now: its words and what its
// y/N asks about; the factory checks a bound y against it. The others do
// their request and answer in words; an error is a refusal, its text your
// answer.
type PRWorker interface {
	Plan(ctx context.Context, kind string, pr PRRef) (FixPlan, error)
	Fix(ctx context.Context, p FixPayload) (string, error)
	SwitchFix(ctx context.Context, p FixSwitchPayload, off bool) (string, error)
	FixDone(ctx context.Context, p FixDonePayload) (string, error)
	FixDecide(ctx context.Context, p FixDecidePayload) (string, error)
	FixCI(ctx context.Context, p FixCIPayload) (string, error)
	MergeParent(ctx context.Context, p MergeParentPayload) (string, error)
	StopFix(ctx context.Context, p FixStopPayload) (string, error)
	Handoff(ctx context.Context, p HandoffPayload) (string, error)
}

// fixAskedWait is how long the y/N of a PR request's plan binds it, as long
// as magnum's binds a merge of the parent: a plan asked about that long ago
// or longer is refused, since the PR or its base may have moved under a
// question left open.
const fixAskedWait = 10 * time.Minute

// maxReplyBytes bounds one reply of yours (FixDecision.Reply): GitHub's own
// bound on a comment's body.
const maxReplyBytes = 65536

// errFixOff is every PR request's answer while [factory] fix is off.
var errFixOff = fmt.Errorf("the factory does not fix PRs yet: [factory] fix is %q, so nothing was done", config.FactoryFixOff)

// errNoFixLane is a PR request's answer from a factory with [factory] fix
// on and no fix lane (Deps.PRWork nil).
var errNoFixLane = errors.New("this factory build does not fix PRs yet (its fix lane comes in a later build): nothing was done")

// prRequest answers request q, one of FixKinds.
func (e *Engine) prRequest(ctx context.Context, q registry.Request) {
	switch q.Kind {
	case ReqFix:
		respond(e, ctx, q, e.fixRequest)
	case ReqFixOff, ReqFixOn:
		respond(e, ctx, q, func(ctx context.Context, p FixSwitchPayload) (string, error) {
			return prDo(e, p.PRRef, nil, func(w PRWorker) (string, error) { return w.SwitchFix(ctx, p, q.Kind == ReqFixOff) })
		})
	case ReqFixDone:
		respond(e, ctx, q, func(ctx context.Context, p FixDonePayload) (string, error) {
			return prDo(e, p.PRRef, seenCheck(p.Seen), func(w PRWorker) (string, error) { return w.FixDone(ctx, p) })
		})
	case ReqFixDecide:
		respond(e, ctx, q, func(ctx context.Context, p FixDecidePayload) (string, error) {
			return prDo(e, p.PRRef, decideCheck(p.Decide), func(w PRWorker) (string, error) { return w.FixDecide(ctx, p) })
		})
	case ReqFixCI:
		respond(e, ctx, q, func(ctx context.Context, p FixCIPayload) (string, error) {
			return e.boundRequest(ctx, q.Kind, p.PRRef, p.DryRun, p.Asked, false, func(w PRWorker) (string, error) { return w.FixCI(ctx, p) })
		})
	case ReqMergeParent:
		respond(e, ctx, q, func(ctx context.Context, p MergeParentPayload) (string, error) {
			return e.boundRequest(ctx, q.Kind, p.PRRef, p.DryRun, p.Asked, false, func(w PRWorker) (string, error) { return w.MergeParent(ctx, p) })
		})
	case ReqFixStop:
		respond(e, ctx, q, func(ctx context.Context, p FixStopPayload) (string, error) {
			return prDo(e, p.PRRef, nil, func(w PRWorker) (string, error) { return w.StopFix(ctx, p) })
		})
	case ReqHandoff:
		respond(e, ctx, q, func(ctx context.Context, p HandoffPayload) (string, error) {
			return e.boundRequest(ctx, q.Kind, p.PRRef, p.DryRun, p.Asked, true, func(w PRWorker) (string, error) { return w.Handoff(ctx, p) })
		})
	}
}

// fixRequest answers a ReqFix: its plan on a dry run, else the start.
func (e *Engine) fixRequest(ctx context.Context, p FixPayload) (string, error) {
	return e.boundRequest(ctx, ReqFix, p.PRRef, p.DryRun, nil, false, func(w PRWorker) (string, error) { return w.Fix(ctx, p) })
}

// prDo is a PR request on pr as the factory takes it: refused while
// [factory] fix is off (errFixOff), with no fix lane (errNoFixLane), for a
// PR the request does not name and for check's refusal (nil: none); else
// do on the fix lane.
func prDo(e *Engine, pr PRRef, check error, do func(PRWorker) (string, error)) (string, error) {
	switch {
	case !e.cfg.Factory.Fixes():
		return "", errFixOff
	case e.d.PRWork == nil:
		return "", errNoFixLane
	}
	if err := prCheck(pr); err != nil {
		return "", err
	}
	if check != nil {
		return "", check
	}
	return do(e.d.PRWork)
}

// boundRequest is prDo for a request of kind that has a plan: a dry run
// answers it (FixPlan, the worker's), and asked, the plan the y/N named,
// binds the request to it: refused when what it names moved since
// (askedRefusal, against the worker's plan now). need says the request
// takes no y without its plan (a hand-off); a dry run binds nothing.
func (e *Engine) boundRequest(ctx context.Context, kind string, pr PRRef, dry bool, asked *FixAsked, need bool,
	do func(PRWorker) (string, error)) (string, error) {
	return prDo(e, pr, askedCheck(kind, dry, asked, need), func(w PRWorker) (string, error) {
		if !dry && asked == nil {
			return do(w)
		}
		plan, err := w.Plan(ctx, kind, pr)
		if err != nil {
			return "", err
		}
		plan.Ref, plan.Kind = prLabel(pr), kind
		if dry {
			return plan.result()
		}
		if why := askedRefusal(prLabel(pr), *asked, plan.Asked, e.now()); why != "" {
			return "", errors.New(why)
		}
		return do(w)
	})
}

// prLabel is pr as the answers name it: owner/name#N.
func prLabel(pr PRRef) string { return fmt.Sprintf("%s#%d", pr.Repo, pr.Number) }

// repoPartRe is one part of a repository's owner/name as GitHub spells it
// (github's checkRepo), "." and ".." aside.
var repoPartRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// prCheck refuses a request that names no pull request: its repository
// is owner/name, each part of [A-Za-z0-9._-] alone and neither "." nor
// ".." (no path part, control character or escape reaches the fix lane,
// which may join the name into a path or echo it), and its number
// positive.
func prCheck(pr PRRef) error {
	owner, name, ok := strings.Cut(pr.Repo, "/")
	if !ok || !repoPart(owner) || !repoPart(name) || pr.Number <= 0 {
		return fmt.Errorf("the request names no pull request (%q#%d): it needs the repository's owner/name and the PR's number", pr.Repo, pr.Number)
	}
	return nil
}

// repoPart says whether s is an owner or a name GitHub could give.
func repoPart(s string) bool { return repoPartRe.MatchString(s) && s != "." && s != ".." }

// seenCheck refuses a ReqFixDone that names no attention you saw.
func seenCheck(seen string) error {
	if strings.TrimSpace(seen) == "" {
		return errors.New("the request names no attention you saw: say you did what a fix loop left you from a screen that shows it")
	}
	return nil
}

// decideCheck refuses a ReqFixDecide with no reply, a reply that names no
// thread or says nothing, a thread named twice, or a reply longer than
// GitHub takes.
func decideCheck(ds []FixDecision) error {
	if len(ds) == 0 {
		return errors.New("the request carries no reply: --decide names a thread and your reply there")
	}
	seen := map[int64]bool{}
	for _, d := range ds {
		switch {
		case d.Thread <= 0:
			return fmt.Errorf("a reply names no thread (%d)", d.Thread)
		case strings.TrimSpace(d.Reply) == "":
			return fmt.Errorf("the reply in thread %d says nothing", d.Thread)
		case len(d.Reply) > maxReplyBytes:
			return fmt.Errorf("the reply in thread %d is longer than GitHub takes (%d bytes, at most %d)", d.Thread, len(d.Reply), maxReplyBytes)
		case seen[d.Thread]:
			return fmt.Errorf("thread %d is named twice", d.Thread)
		}
		seen[d.Thread] = true
	}
	return nil
}

// askedCheck refuses a binding a request of kind cannot take: a dry run
// that carries one (it binds nothing), a hand-off without one (need), and
// one that names no plan of its kind (no head or time; a merge without its
// base and tip, a CI fix without its failed entries, a hand-off without
// whom it goes to).
func askedCheck(kind string, dry bool, a *FixAsked, need bool) error {
	switch {
	case dry && a != nil:
		return errors.New("a dry run binds nothing: send the plan back with the y alone")
	case dry:
		return nil
	case a == nil && need:
		return errors.New("a hand-off assigns a person, so it needs the plan its y/N asked about: ask again from the screen")
	case a == nil:
		return nil
	}
	short := ""
	switch {
	case a.Head == "" || a.At.IsZero():
		short = "its head and its time"
	case kind == ReqMergeParent && (a.Base == "" || a.Tip == ""):
		short = "the parent and its tip"
	case kind == ReqFixCI && len(a.Entries) == 0:
		short = "the failed checks"
	case kind == ReqHandoff && a.Assignee == "":
		short = "whom the PR goes to"
	}
	if short != "" {
		return fmt.Errorf("the y names no plan it was asked about (no %s): nothing was done; ask again", short)
	}
	return nil
}

// askedRefusal says why label's request does not run as its y/N asked
// (asked) at now, when its plan is now plan: a question fixAskedWait or
// longer ago, or what moved since: the PR's head, its base (a retarget),
// the base's tip, the merge's conflicts or the merge the factory holds,
// the failed checks or the CI fix's count, or whom the PR goes to; "" when
// nothing did.
func askedRefusal(label string, asked, plan FixAsked, now time.Time) string {
	const nothing = " since you asked: nothing was done; ask again"
	if age := now.Sub(asked.At); age >= fixAskedWait {
		return fmt.Sprintf("%s: asked %s ago, longer than a y binds its plan (%s): nothing was done; ask again", label,
			textx.DurationWords(age.Truncate(time.Minute)), textx.DurationWords(fixAskedWait))
	}
	short := textx.ShortSHA
	switch {
	case !strings.EqualFold(asked.Head, plan.Head):
		return fmt.Sprintf("%s: its head moved from %s to %s", label, short(asked.Head), short(plan.Head)) + nothing
	case asked.Base != plan.Base:
		return fmt.Sprintf("%s: its base moved from %s to %s", label, asked.Base, plan.Base) + nothing
	case !strings.EqualFold(asked.Tip, plan.Tip):
		return fmt.Sprintf("%s: %s moved from %s to %s", label, plan.Base, short(asked.Tip), short(plan.Tip)) + nothing
	case asked.Conflicts != plan.Conflicts:
		return fmt.Sprintf("%s: the merge conflicts in %d files now, and you were asked about %d", label, plan.Conflicts, asked.Conflicts) + nothing
	case !strings.EqualFold(asked.Merge, plan.Merge):
		return fmt.Sprintf("%s: the factory holds the merge %s now, and you were asked about %s", label, short(plan.Merge), short(asked.Merge)) +
			nothing
	case !slices.Equal(asked.Entries, plan.Entries) || asked.Count != plan.Count:
		return fmt.Sprintf("%s: the failed checks are %s (CI fix %d) now, and you were asked about %s (CI fix %d)", label,
			strings.Join(plan.Entries, ", "), plan.Count, strings.Join(asked.Entries, ", "), asked.Count) + nothing
	case !strings.EqualFold(asked.Assignee, plan.Assignee):
		return fmt.Sprintf("%s: it goes to %s now, and you were asked about %s", label, plan.Assignee, asked.Assignee) + nothing
	}
	return ""
}

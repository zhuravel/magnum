package staging

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// editJournal changes effect id's journal on disk, as a hand (or a
// corruption) would between two Steps.
func (w *deployWorld) editJournal(id EffectID, edit func(j *journal)) {
	w.t.Helper()
	root, err := openDeploys(w.mirror, false)
	if err != nil {
		w.t.Fatal(err)
	}
	defer root.Close()
	j, err := readJournal(root, id)
	if err != nil {
		w.t.Fatal(err)
	}
	edit(j)
	if err := writeJournal(root, j); err != nil {
		w.t.Fatal(err)
	}
}

// journalOf reads effect id's journal.
func (w *deployWorld) journalOf(id EffectID) *journal {
	w.t.Helper()
	root, err := openDeploys(w.mirror, false)
	if err != nil {
		w.t.Fatal(err)
	}
	defer root.Close()
	j, err := readJournal(root, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return j
}

// failEvents counts the fail events recorded since n.
func (w *deployWorld) failEvents(since int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := 0
	for _, ev := range w.events[since:] {
		if strings.Fields(ev)[1] == PhaseFail {
			k++
		}
	}
	return k
}

func (w *deployWorld) eventCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.events)
}

// I1. Right before the push, the merge is checked again from what the
// mirror and the remote say now, never from the journal alone: a journal
// whose merge was edited to a commit that wipes the branch, a tag of the
// real merge, a commit with the right parents and another tree, or whose
// env was edited to another configured env, fails at push, and nothing
// is pushed anywhere.
func TestThePushChecksTheMergeAgainFromTheLiveState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(w *deployWorld, j *journal)
		why  string
	}{
		{"an empty-tree commit on the tip", func(w *deployWorld, j *journal) {
			j.Merge = gitIn(w.t, w.r, w.mirror, "commit-tree", emptyTree, "-p", w.tip, "-m", "wipe")
		}, "parents"},
		{"the right parents and another tree", func(w *deployWorld, j *journal) {
			j.Merge = gitIn(w.t, w.r, w.mirror, "commit-tree", emptyTree, "-p", w.tip, "-p", w.heads[1], "-m", "wipe")
		}, "tree"},
		{"a tag of the merge", nil, "not a commit"},
		{"another configured env", func(w *deployWorld, j *journal) {
			j.Env = bart
		}, "id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			gitIn(t, w.r, w.work, "push", "-q", "origin", w.c1+":refs/heads/staging-bart")
			w.deps.Stagings = func(string) []Env { return []Env{void, bart} }
			c := w.client()
			defer c.Close()
			id := w.deploy(c, 1)
			e := w.drive(c, id, at(StepPush))
			if tc.edit == nil { // a tag of the merge
				tag := fmt.Sprintf("object %s\ntype commit\ntag v-probe\ntagger T <t@example.com> 0 +0000\n\nprobe\n", e.Merge)
				res, err := w.r.Run(context.Background(), execx.Cmd{Name: "git", Args: []string{"-C", w.mirror, "mktag"}, Stdin: []byte(tag)})
				if err != nil {
					t.Fatal(err)
				}
				w.editJournal(id, func(j *journal) { j.Merge = strings.TrimSpace(res.Out()) })
			} else {
				w.editJournal(id, func(j *journal) { tc.edit(w, j) })
			}
			e = w.drive(c, id, ended)
			if e.State != EffectFailed || e.Step != StepPush || !strings.Contains(e.Why, tc.why) || !strings.Contains(e.Why, "nothing was pushed") {
				t.Errorf("ended: %+v, want failed at push, %q, nothing pushed", e, tc.why)
			}
			w.noPushSince(0, w.tip)
			if got := gitIn(t, w.r, w.origin, "rev-parse", "refs/heads/staging-bart"); got != w.c1 {
				t.Errorf("staging-bart moved to %s", short(got))
			}
		})
	}
}

// emptyTree is git's empty tree.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// bart is a second staging env.
var bart = Env{Name: "bart", Branch: "staging-bart"}

// The retry backoff's ruling: a transient failure at a step tries again
// after 30 s, 1, 2, 4 and 8 minutes, the journal's Why naming the next
// try's time; a Step before that time does nothing; the failure ends the
// effect only once it has failed for failBudget, failed before the push,
// done, "pushed, not confirmed", after it (the push stands). The env is
// released, and the end is the one fail event.
func TestATransientFailureBacksOffAndEndsAfterFifteenMinutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		at     string // the step the failure starts at
		break_ func(w *deployWorld)
		state  string
	}{
		{"the PR's pull ref is gone", StepPlan, func(w *deployWorld) {
			gitIn(w.t, w.r, w.origin, "update-ref", "-d", "refs/pull/1/head")
		}, EffectFailed},
		{"merge-tree fails", StepMerge, func(w *deployWorld) { w.run.failing("merge-tree") }, EffectFailed},
		{"the fetch fails at verify", StepVerify, func(w *deployWorld) { w.run.failing("fetch") }, EffectFailed},
		{"the default branch cannot be read at push", StepPush, func(w *deployWorld) { w.run.failing("symbolic-ref") }, EffectFailed},
		{"the tip's history cannot be read at push", StepPush, func(w *deployWorld) { w.run.failing("merge-base") }, EffectFailed},
		{"ci.yml's runs cannot be read", StepWatch, func(w *deployWorld) {
			w.gh.mu.Lock()
			w.gh.runsErr = errors.New("502 from GitHub")
			w.gh.mu.Unlock()
		}, EffectDone},
		{"the deployments cannot be read", StepConfirm, func(w *deployWorld) {
			w.gh.mu.Lock()
			w.gh.deployErr["void"] = errors.New("502 from GitHub")
			w.gh.mu.Unlock()
		}, EffectDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newDeployWorld(t)
			c := w.client()
			defer c.Close()
			id := w.deploy(c, 1)
			e := w.drive(c, id, func(e Effect) bool {
				if e.Step == StepWatch && tc.at == StepConfirm {
					w.passCI(e.Merge)
				}
				return e.Step == tc.at || ended(e)
			})
			if ended(e) {
				t.Fatalf("ended before %s: %+v", tc.at, e)
			}
			tc.break_(w)
			events := w.eventCount()
			start := w.deps.Now()
			var waits []time.Duration
			for range 20 {
				if err := c.Step(context.Background()); err != nil {
					t.Fatalf("Step: %v", err)
				}
				if e = w.effect(c, id); ended(e) {
					break
				}
				j := w.journalOf(id)
				if !j.RetryAt.After(w.deps.Now()) || !strings.Contains(e.Why, "next try at "+j.RetryAt.Format("15:04:05")) {
					t.Fatalf("after a failure: %+v, retry at %s; want a later try named in Why", e, j.RetryAt)
				}
				seq := j.Seq
				if err := c.Step(context.Background()); err != nil {
					t.Fatal(err)
				}
				if w.journalOf(id).Seq != seq {
					t.Fatal("a Step before the next try's time ran the step")
				}
				waits = append(waits, j.RetryAt.Sub(w.deps.Now()))
				w.advance(j.RetryAt.Sub(w.deps.Now()))
			}
			want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
			if !slices.Equal(waits, want) {
				t.Errorf("the waits between tries: %v, want %v", waits, want)
			}
			if failing := w.deps.Now().Sub(start); failing < failBudget {
				t.Errorf("ended after %s of failing, want at least %s", failing, failBudget)
			}
			if e.State != tc.state || e.Step != tc.at || !strings.Contains(e.Why, tc.at+" failed for") {
				t.Errorf("ended: %+v, want %s at %s, %q", e, tc.state, tc.at, tc.at+" failed for")
			}
			if tc.state == EffectDone && (!strings.HasPrefix(e.Why, NotConfirmed) || !strings.Contains(e.Why, "the push stands")) {
				t.Errorf("after the push: %q, want pushed, not confirmed, the push stands", e.Why)
			}
			if _, held := c.heldFor(w.mirror, "void"); held {
				t.Error("the env is still held")
			}
			if got := w.failEvents(events); got != 1 {
				t.Errorf("fail events: %d, want the end's alone", got)
			}
			if tc.state == EffectFailed {
				w.noPushSince(0, w.tip)
			}
		})
	}
}

// A short outage costs no redeploy: the fetch fails four times (over 3
// minutes), the effect tries again after its waits, and it goes on to done
// with its failures forgotten.
func TestAShortOutageIsRiddenOut(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 1)
	w.drive(c, id, at(StepVerify))
	w.run.failing("fetch")
	for range 4 {
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if e := w.effect(c, id); ended(e) {
			t.Fatalf("a short outage ended the effect: %+v", e)
		}
		if at := w.journalOf(id).RetryAt; at.After(w.deps.Now()) {
			w.advance(at.Sub(w.deps.Now()))
		}
	}
	w.run.mu.Lock()
	w.run.fail = nil
	w.run.mu.Unlock()
	if e := w.finish(c, id); e.State != EffectDone || !strings.HasPrefix(e.Why, "live") {
		t.Fatalf("after the outage: %+v, want done, live", e)
	}
	if j := w.journalOf(id); j.Fails != 0 || !j.RetryAt.IsZero() {
		t.Errorf("fails %d, retry at %s; want them forgotten", j.Fails, j.RetryAt)
	}
}

// I2. A resolver whose Result keeps failing, or whose job runs past
// resolveWait, ends the effect failed at merge, the job cancelled.
func TestAResolverThatNeverAnswersEndsTheEffect(t *testing.T) {
	t.Parallel()
	t.Run("Result errors", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		res := w.withResolver()
		res.resultErr = errors.New("the resolver's registry is locked")
		c := w.client()
		defer c.Close()
		id := w.deploy(c, 2)
		start := w.deps.Now()
		e := w.drive(c, id, ended)
		if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, "merge failed for") || w.deps.Now().Sub(start) < failBudget {
			t.Errorf("ended: %+v after %s, want failed at merge after %s", e, w.deps.Now().Sub(start), failBudget)
		}
		if got := res.cancels(); !slices.Equal(got, []string{"job-1"}) {
			t.Errorf("cancels: %q, want job-1", got)
		}
	})
	t.Run("runs too long", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		res := w.withResolver()
		c := w.client()
		defer c.Close()
		id := w.deploy(c, 2)
		w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 })
		w.advance(resolveWait - time.Minute)
		if e := w.drive(c, id, func(Effect) bool { return true }); e.State != EffectRunning {
			t.Fatalf("before the wait: %+v", e)
		}
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		w.advance(time.Minute)
		e := w.drive(c, id, ended)
		if e.State != EffectFailed || !strings.Contains(e.Why, "resolver") || !slices.Equal(res.cancels(), []string{"job-1"}) {
			t.Errorf("ended: %+v, cancels %q; want failed, the job cancelled", e, res.cancels())
		}
	})
	t.Run("CI runs too long", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		id := w.deploy(c, 1)
		e := w.drive(c, id, at(StepWatch))
		w.gh.mu.Lock()
		w.gh.runs[e.Merge] = []Run{{ID: 8, URL: runURL, Status: "in_progress"}}
		w.gh.mu.Unlock()
		w.advance(watchFor)
		if e := w.drive(c, id, at(StepConfirm)); e.State != EffectRunning {
			t.Fatalf("after watchFor: %+v, want at confirm", e)
		}
		w.advance(ConfirmAfterCI)
		e = w.drive(c, id, ended)
		if e.State != EffectDone || !strings.HasPrefix(e.Why, NotConfirmed) || !strings.Contains(e.Why, "still ran") {
			t.Errorf("ended: %+v, want done, not confirmed, ci.yml still ran", e)
		}
	})
}

// I2. The clean merge has a ref in the mirror for the effect's life, so a
// prune while no daemon runs keeps it; a merge the mirror lacks all the
// same fails the effect at once, with no retry and nothing pushed.
func TestAMergeTheMirrorLacksFailsAtOnce(t *testing.T) {
	t.Parallel()
	t.Run("a prune keeps the merge", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		first := w.client()
		id := w.deploy(first, 1)
		w.drive(first, id, at(StepPush))
		kill(first)
		gitIn(t, w.r, w.mirror, "prune", "--expire=now")
		second := w.client()
		defer second.Close()
		if e := w.finish(second, id); e.State != EffectDone {
			t.Errorf("after the prune: %+v, want done", e)
		}
		if got := gitIn(t, w.r, w.mirror, "for-each-ref", "refs/magnum/"); got != "" {
			t.Errorf("the effect's refs are left: %s", got)
		}
	})
	t.Run("a merge gone", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		id := w.deploy(c, 1)
		w.drive(c, id, at(StepPush))
		w.editJournal(id, func(j *journal) { j.Merge = strings.Repeat("cd", 20) })
		if err := c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		e := w.effect(c, id)
		if e.State != EffectFailed || !strings.Contains(e.Why, "not in the mirror") {
			t.Errorf("after one Step: %+v, want failed, the merge is not in the mirror", e)
		}
		w.noPushSince(0, w.tip)
	})
}

// I3. A fresh client finds the live effects by itself, scanning mirror_dir:
// a deploy another daemon started and left at push resumes at the fresh
// client's first Step, the queued one after it is claimed next, and a
// fresh client cancels one, with no Refresh or Effects before.
func TestAFreshClientResumesWithoutMeetingTheRepository(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	w.pr(4, "pr4", map[string]string{"app/d.rb": "d\n"})
	w.pr(5, "pr5", map[string]string{"app/e.rb": "e\n"})
	first := w.client()
	running := w.deploy(first, 1)
	queued := w.deploy(first, 4)
	cancelled := w.deploy(first, 5)
	w.drive(first, running, at(StepPush))
	kill(first)
	canceller := w.client()
	defer canceller.Close()
	if err := canceller.Cancel(context.Background(), cancelled); err != nil {
		t.Fatalf("a fresh client's Cancel: %v", err)
	}
	fresh := w.client()
	defer fresh.Close()
	if err := fresh.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j := w.journalOf(running); j.Step == StepPush && j.Phase == "" {
		t.Fatalf("the fresh client's first Step left the effect at %s %q", j.Step, j.Phase)
	}
	if e := w.finish(fresh, running); e.State != EffectDone {
		t.Fatalf("the resumed effect: %+v", e)
	}
	if e := w.finish(fresh, queued); e.State != EffectDone {
		t.Fatalf("the queued effect: %+v", e)
	}
	if e := w.effect(fresh, cancelled); e.State != EffectCancelled {
		t.Errorf("the cancelled effect: %+v", e)
	}
}

// M1. A tag object as the resolver's merge is refused at once: no commit,
// no retry, nothing pushed.
func TestATagAsTheResolversMergeIsRefused(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	res := w.withResolver()
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 2)
	w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 })
	dir, merge := w.resolverRepo(2, []string{w.tip, w.heads[2]}, map[string]string{"app/s.rb": "resolved\n"})
	gitIn(t, w.r, dir, "tag", "-a", "-m", "tagged merge", "v-probe", merge)
	res.answer(ResolveResult{State: ResolveDone, MergeSHA: gitIn(t, w.r, dir, "rev-parse", "v-probe"), Dir: dir})
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := w.effect(c, id)
	if e.State != EffectFailed || e.Step != StepMerge || !strings.Contains(e.Why, "not a commit") {
		t.Errorf("after one Step: %+v, want failed at merge, not a commit", e)
	}
	w.noPushSince(0, w.tip)
}

// M2. An env name the effect id cannot carry (a leading dash, over 32
// characters, a dot) is refused by the allowlist, so Plan refuses it and
// no effect is ever written that Effects and Step would not see.
func TestAnEnvNameOutsideTheIdRuleIsRefused(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"-x", strings.Repeat("v", 33), "a.b", ""} {
		env := Env{Name: name, Branch: "staging-" + name}
		c := New(Deps{Stagings: func(string) []Env { return []Env{env} }})
		if err := c.allowed("acme/app", env, "master"); err == nil {
			t.Errorf("allowed(%q) passed", name)
		}
	}
	long := Env{Name: strings.Repeat("v", 32), Branch: "staging-" + strings.Repeat("v", 32)}
	if id := newID(long.Name, 99999, time.Now()); !validID(id) {
		t.Errorf("the longest env name makes the id %q, which validID refuses", id)
	}
	w := newDeployWorld(t)
	bad := Env{Name: "-x", Branch: "staging--x"}
	w.deps.Stagings = func(string) []Env { return []Env{void, bad} }
	c := w.client()
	defer c.Close()
	if _, err := c.Plan(context.Background(), w.prOf(1), []Env{bad}); err == nil {
		t.Error("Plan took an env whose name the id cannot carry")
	}
}

// M3. A push that fails says git's own reason (the remote's refusal), never
// the command line with its options.
func TestAPushFailureSaysGitsReasonNotTheArgv(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	writeIn(t, w.origin, "hooks/pre-receive", "#!/bin/sh\necho 'denied by the test origin' >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(w.origin, "hooks", "pre-receive"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := w.client()
	defer c.Close()
	e := w.drive(c, w.deploy(c, 1), ended)
	if e.State != EffectFailed || !strings.Contains(e.Why, "denied by the test origin") || strings.Contains(e.Why, "--git-dir") || strings.Contains(e.Why, "credential") {
		t.Errorf("ended: %+v, want failed with the remote's words and no argv", e)
	}
}

// M4. A head already on the branch is "already there": Plan says so and
// Deploy refuses it, and an effect whose head lands meanwhile ends done
// without an empty merge. Two Deploys of the same PR and head at once
// start one effect.
func TestAHeadAlreadyOnTheBranchIsAlreadyThere(t *testing.T) {
	t.Parallel()
	t.Run("plan", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		w.finish(c, w.deploy(c, 1))
		pushes := w.run.pushed()
		p := w.plan(c, 1)
		if got := p.Envs[0].Why; !strings.Contains(got, "already there") {
			t.Fatalf("plan again: %+v, want already there", p.Envs[0])
		}
		if _, err := c.Deploy(context.Background(), p, ByCLI); err == nil {
			t.Error("Deploy of a head already there started")
		}
		if w.run.pushed() != pushes {
			t.Error("pushed again")
		}
	})
	t.Run("landed meanwhile", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		c.hook = func(_ EffectID, step, point string) error {
			if step == StepPlan && point == "begin" {
				git := func(args ...string) string { t.Helper(); return gitIn(t, w.r, w.work, args...) }
				git("fetch", "-q", "origin", "staging-void")
				git("checkout", "-q", "--detach", "FETCH_HEAD")
				git("merge", "-q", "--no-ff", "-m", "a coworker merged pr1", w.heads[1])
				git("push", "-q", "origin", "HEAD:refs/heads/staging-void")
				git("checkout", "-q", "master")
			}
			return nil
		}
		id := w.deploy(c, 1)
		e := w.drive(c, id, ended)
		if e.State != EffectDone || !strings.Contains(e.Why, "already there") || w.run.pushed() != 0 || w.run.committed() != 0 {
			t.Errorf("ended: %+v (%d pushes, %d commits), want done, already there, no merge", e, w.run.pushed(), w.run.committed())
		}
	})
	t.Run("two at once", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		a, b := w.client(), w.client()
		defer a.Close()
		defer b.Close()
		p := w.plan(a, 1)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, c := range []*Client{a, b} {
			wg.Go(func() { _, errs[i] = c.Deploy(context.Background(), p, ByCLI) })
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Errorf("two Deploys at once: %v, %v; want exactly one refused", errs[0], errs[1])
		}
		if es := a.Effects("acme/app"); len(es) != 1 {
			t.Errorf("effects: %d, want 1", len(es))
		}
	})
}

// M5. A resumed needs-you effect starts with fresh counters, the
// resolver's tries included; a Deploy that cannot write every journal
// starts none.
func TestADeployStartsAllOrNoneAndAResumeStartsFresh(t *testing.T) {
	t.Parallel()
	t.Run("fresh counters", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		c := w.client()
		defer c.Close()
		id := w.deploy(c, 1)
		w.drive(c, id, at(StepMerge))
		w.editJournal(id, func(j *journal) {
			j.State, j.Why, j.Resolves, j.Remerges, j.Fails = EffectNeedsYou, "needs you", maxResolves, 1, 2
		})
		again, err := c.Deploy(context.Background(), w.plan(c, 1), ByCLI)
		if err != nil || again[0] != id {
			t.Fatalf("resume: %v %v", again, err)
		}
		if j := w.journalOf(id); j.Resolves != 0 || j.Remerges != 0 || j.Fails != 0 {
			t.Errorf("resumed with resolves %d, remerges %d, fails %d; want 0", j.Resolves, j.Remerges, j.Fails)
		}
	})
	t.Run("all or none", func(t *testing.T) {
		t.Parallel()
		w := newDeployWorld(t)
		gitIn(t, w.r, w.work, "push", "-q", "origin", w.tip+":refs/heads/staging-bart")
		w.deps.Stagings = func(string) []Env { return []Env{void, bart} }
		c := w.client()
		defer c.Close()
		p, err := c.Plan(context.Background(), w.prOf(1), []Env{void, bart})
		if err != nil {
			t.Fatal(err)
		}
		writes := 0
		c.hook = func(_ EffectID, step, point string) error {
			if step == "deploy" && point == "written" {
				if writes++; writes == 2 {
					return errors.New("the disk is full")
				}
			}
			return nil
		}
		if ids, err := c.Deploy(context.Background(), p, ByCLI); err == nil {
			t.Fatalf("Deploy: %v, want the write's error", ids)
		}
		if es := c.Effects("acme/app"); len(es) != 0 {
			t.Errorf("effects after a Deploy that failed: %+v, want none", es)
		}
	})
}

// M6. A path a PR names reaches Why and the events without its control
// characters: an escape sequence in a conflicted file's name is quoted.
func TestPRPathsAreSanitizedInWhyAndEvents(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	odd := "app/\x1b[31mred.rb"
	w.coworker(map[string]string{odd: "staging\n"})
	w.pr(3, "pr3", map[string]string{odd: "theirs\n"})
	c := w.client()
	defer c.Close()
	e := w.drive(c, w.deploy(c, 3), ended)
	if e.State != EffectFailed || !strings.Contains(e.Why, "conflict") {
		t.Fatalf("ended: %+v, want the conflict", e)
	}
	if strings.ContainsRune(e.Why, 0x1b) || !strings.Contains(e.Why, `\x1b`) {
		t.Errorf("Why %q holds the raw escape, or does not show it quoted", e.Why)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ev := range w.events {
		if strings.ContainsRune(ev, 0x1b) {
			t.Errorf("an event holds the raw escape: %q", ev)
		}
	}
}

// M7. The resolver fetch pins protocol v2 and hands upload-pack, the child
// that reads the resolver's repository, its own command-line options
// (no hooks, any sha), which git's local transport would otherwise drop.
func TestTheResolverFetchPinsItsOptionsOnUploadPack(t *testing.T) {
	t.Parallel()
	rec := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}}}}
	g := newGit(rec, func(string) string { return "file:///origin" }).(*execGit)
	mirror := filepath.Join(t.TempDir(), "acme", "app.git")
	sha := strings.Repeat("a", 40)
	if err := g.fetchResolved(context.Background(), mirror, "/tmp/resolver/.git", sha, resolvedRef("void-1-20261108t120000-abcdef01")); err != nil {
		t.Fatal(err)
	}
	if len(rec.Calls) != 1 {
		t.Fatalf("calls: %d", len(rec.Calls))
	}
	args := rec.Calls[0].Args
	if !slices.Contains(args, "protocol.version=2") {
		t.Errorf("no protocol v2 pinned: %q", args)
	}
	i := slices.IndexFunc(args, func(a string) bool { return strings.HasPrefix(a, "--upload-pack=") })
	if i < 0 {
		t.Fatalf("no --upload-pack: %q", args)
	}
	up := args[i]
	for _, want := range []string{"git ", "-c core.hooksPath=/dev/null", "-c uploadpack.allowAnySHA1InWant=true", " upload-pack"} {
		if !strings.Contains(up, want) {
			t.Errorf("--upload-pack %q lacks %q", up, want)
		}
	}
}

// M8. A cancelled ci.yml run on the merge goes on to confirm, and the end
// state says it was cancelled.
func TestACancelledCIRunIsKeptInTheEndState(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	c := w.client()
	defer c.Close()
	id := w.deploy(c, 1)
	e := w.drive(c, id, at(StepWatch))
	w.gh.mu.Lock()
	w.gh.runs[e.Merge] = []Run{{ID: 8, URL: runURL, Status: "completed", Conclusion: "cancelled"}}
	w.gh.mu.Unlock()
	w.drive(c, id, at(StepConfirm))
	w.deployed(e.Merge)
	e = w.drive(c, id, ended)
	if e.State != EffectDone || !strings.Contains(e.Why, "cancelled") {
		t.Errorf("ended: %+v, want done, saying ci.yml's run was cancelled", e)
	}
}

// lockFree fails the test when the mirror's flock is held: a call made
// under it would hold up every Refresh.
func (w *deployWorld) lockFree(what string) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	unlock, err := lockMirror(ctx, w.mirror, nil)
	if err != nil {
		w.t.Errorf("%s ran under the mirror's flock", what)
		return
	}
	unlock()
}

// lockCheckingGitHub is the fake GitHub, checking the mirror's flock is
// free at each call.
type lockCheckingGitHub struct {
	*fakeGitHub
	w *deployWorld
}

func (g lockCheckingGitHub) DeploymentsOf(ctx context.Context, repo, env, etag string) (Deployments, error) {
	g.w.lockFree("DeploymentsOf")
	return g.fakeGitHub.DeploymentsOf(ctx, repo, env, etag)
}

func (g lockCheckingGitHub) RunsOf(ctx context.Context, repo, workflow, sha string) ([]Run, error) {
	g.w.lockFree("RunsOf")
	return g.fakeGitHub.RunsOf(ctx, repo, workflow, sha)
}

// lockCheckingResolver is the fake resolver, checking the flock likewise.
type lockCheckingResolver struct {
	*fakeResolver
	w *deployWorld
}

func (r lockCheckingResolver) Start(ctx context.Context, req ResolveRequest) (string, error) {
	r.w.lockFree("Resolver.Start")
	return r.fakeResolver.Start(ctx, req)
}

func (r lockCheckingResolver) Result(ctx context.Context, job string) (ResolveResult, error) {
	r.w.lockFree("Resolver.Result")
	return r.fakeResolver.Result(ctx, job)
}

// M9. No GitHub, resolver, PushEnv or CommitIdentity call runs under the
// mirror's flock: a Refresh waits only for git.
func TestNoMirrorLockAcrossGitHubTheResolverOrPushEnv(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	res := w.withResolver()
	w.deps.GitHub = lockCheckingGitHub{fakeGitHub: w.gh, w: w}
	w.deps.Resolver = lockCheckingResolver{fakeResolver: res, w: w}
	w.deps.PushEnv = func(context.Context, string, string, string) (map[string]string, error) {
		w.lockFree("PushEnv")
		return map[string]string{}, nil
	}
	w.deps.CommitIdentity = func(context.Context, string) (string, string, error) {
		w.lockFree("CommitIdentity")
		return botName, botEmail, nil
	}
	c := w.client()
	defer c.Close()
	if e := w.finish(c, w.deploy(c, 1)); e.State != EffectDone {
		t.Fatalf("clean: %+v", e)
	}
	id := w.deploy(c, 2)
	w.drive(c, id, func(e Effect) bool { return len(res.requests()) == 1 })
	if err := c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// M10. An event whose journal write landed but whose delivery a crash cut
// is delivered when the effect resumes (at least once): the merge's begin,
// cut right after its save, reaches Event in the next client, beside the
// resumed begin.
func TestAnEventCutByACrashIsDeliveredOnResume(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	first := w.client()
	hit := stopAt(first, StepMerge, "saved")
	id := w.deploy(first, 1)
	for i := 0; !*hit && i < 5; i++ {
		if err := first.Step(context.Background()); err != nil && !errors.Is(err, errHook) {
			t.Fatal(err)
		}
	}
	kill(first)
	merging := func(ev string) bool { return strings.HasPrefix(ev, "merge begin merging") }
	w.mu.Lock()
	early := slices.ContainsFunc(w.events, merging)
	w.mu.Unlock()
	if !*hit || early {
		t.Fatalf("hit %v, the merge's begin delivered before the crash %v", *hit, early)
	}
	second := w.client()
	defer second.Close()
	if err := second.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := w.effect(second, id); e.State != EffectRunning {
		t.Fatalf("resumed: %+v", e)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !slices.ContainsFunc(w.events, merging) || !slices.ContainsFunc(w.events, func(ev string) bool { return strings.HasPrefix(ev, "merge begin resumed:") }) {
		t.Errorf("events after the resume: %q, want the cut begin and the resumed one", w.events)
	}
}

// The review's P4: a second client cancels an effect while the first holds
// its env: it ends cancelled, the first lets the env go at its next Step,
// and nothing is pushed.
func TestACancelFromAnotherClientEndsTheEffectTheFirstHolds(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	a, b := w.client(), w.client()
	defer a.Close()
	defer b.Close()
	id := w.deploy(a, 1)
	w.drive(a, id, at(StepVerify))
	if err := b.Cancel(context.Background(), id); err != nil {
		t.Fatalf("b's Cancel: %v", err)
	}
	for range 3 {
		if err := a.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if e := w.effect(a, id); e.State != EffectCancelled {
		t.Errorf("after b's cancel: %+v, want cancelled", e)
	}
	if _, held := a.heldFor(w.mirror, "void"); held {
		t.Error("a still holds the env")
	}
	w.noPushSince(0, w.tip)
}

// The mirror root: Step and Cancel scan Deps.MirrorRoot alone. Without it,
// a fresh client finds no effect, whatever Mirror says; with it, it finds
// them all.
func TestTheScanUsesTheMirrorRootAlone(t *testing.T) {
	t.Parallel()
	w := newDeployWorld(t)
	first := w.client()
	id := w.deploy(first, 1)
	w.drive(first, id, at(StepPush))
	kill(first)
	noRoot := w.deps
	noRoot.MirrorRoot = nil
	blind := New(noRoot)
	defer blind.Close()
	if err := blind.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j := w.journalOf(id); j.Step != StepPush || j.Phase != "" {
		t.Errorf("a client without MirrorRoot ran the effect: %s %q", j.Step, j.Phase)
	}
	if err := blind.Cancel(context.Background(), id); err == nil {
		t.Error("a client without MirrorRoot found the effect to cancel")
	}
	unmounted := w.deps
	unmounted.MirrorRoot = func() (string, bool) { return filepath.Dir(filepath.Dir(w.mirror)), false }
	if err := New(unmounted).Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j := w.journalOf(id); j.Step != StepPush || j.Phase != "" {
		t.Errorf("a client whose root is not mounted ran the effect: %s %q", j.Step, j.Phase)
	}
	seeing := w.client()
	defer seeing.Close()
	if e := w.finish(seeing, id); e.State != EffectDone {
		t.Errorf("with MirrorRoot: %+v, want done", e)
	}
}

package engine

// A staging deploy's events in magnum's registry (staging's Deps.Event):
// each step's begin, ok and fail, delivered at least once from the
// effect's outbox (also after a restart, and for an effect that ended), is
// written once, keyed by (effect, Seq, step, phase), on the PR's subject,
// as kind staging.deploy. Each effect's record (KVStagingDeploy) keeps its
// event of the newest Seq, so a late repeat of an older write never turns
// it back: the board's card and `magnum staging` read the records, every
// one in a single read, without git. The tick drops an ended effect's
// record stagingDeployKeep after its last event.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/staging"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVStagingDeploys matches every staging deploy's record (KVStagingDeploy)
// in one KVValues read.
const KVStagingDeploys = "staging.deploy.*"

// KVStagingDeploy is effect id's record: StagingDeployRecord as JSON, its
// event of the newest Seq. The review daemon alone writes it.
func KVStagingDeploy(id string) string { return "staging.deploy." + id }

// stagingDeployKeep is how long an ended deploy's record stays after its
// last event, for the card to say how it ended.
const stagingDeployKeep = 24 * time.Hour

// stagingEventWait bounds one event's write: Event has no ctx (it is
// handed on after the effect's flocks are let go).
const stagingEventWait = 10 * time.Second

// StagingDeployRecord is a staging deploy's effect as its newest event left
// it: the PR (Repo, Number, Head), the env, who started it (staging's By:
// cli, liaison or factory), the step and phase of that event, the effect's
// state and why, the event's words, the merge, ci.yml's run, the Seq of the
// journal write and when the event came.
type StagingDeployRecord struct {
	Effect string    `json:"effect"`
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	Head   string    `json:"head"`
	Env    string    `json:"env"`
	By     string    `json:"by,omitempty"`
	Step   string    `json:"step"`
	Phase  string    `json:"phase"`
	State  string    `json:"state"`
	Why    string    `json:"why,omitempty"`
	Text   string    `json:"text,omitempty"`
	Merge  string    `json:"merge,omitempty"`
	RunURL string    `json:"run,omitempty"`
	Seq    int       `json:"seq"`
	At     time.Time `json:"at"`
}

// Live reports whether the effect still runs: queued or running.
func (r StagingDeployRecord) Live() bool {
	return r.State == staging.EffectQueued || r.State == staging.EffectRunning
}

// Unconfirmed reports whether the effect is done without a deployment
// that holds its merge: its Why starts with staging.NotConfirmed (the
// deployments not readable, or none after ci.yml passed), or it ended as a
// fail of its step after the push. Pushed, not live for sure.
func (r StagingDeployRecord) Unconfirmed() bool {
	return r.State == staging.EffectDone && (r.Phase == staging.PhaseFail || strings.HasPrefix(r.Why, staging.NotConfirmed))
}

// parseStagingDeployRecord reads a KVStagingDeploy value; false when it
// cannot be read or names no effect.
func parseStagingDeployRecord(v string) (StagingDeployRecord, bool) {
	var r StagingDeployRecord
	if json.Unmarshal([]byte(v), &r) != nil || r.Effect == "" {
		return StagingDeployRecord{}, false
	}
	return r, true
}

// StagingDeploysOf is the deploy records of repo#number among values (a
// KVValues read of KVStagingDeploys), the live ones first, each group the
// newest first; their words made safe for a terminal.
func StagingDeploysOf(values map[string]string, repo string, number int) []StagingDeployRecord {
	var out []StagingDeployRecord
	for _, k := range slices.Sorted(maps.Keys(values)) {
		r, ok := parseStagingDeployRecord(values[k])
		if !ok || r.Number != number || !strings.EqualFold(r.Repo, repo) || KVStagingDeploy(r.Effect) != k {
			continue
		}
		r.Env, r.Step, r.Phase, r.State, r.Why, r.Text = stagingClean(r.Env), stagingClean(r.Step), stagingClean(r.Phase),
			stagingClean(r.State), stagingClean(r.Why), stagingClean(r.Text)
		if !strings.HasPrefix(r.RunURL, "https://") || stagingClean(r.RunURL) != r.RunURL || strings.Contains(r.RunURL, " ") {
			r.RunURL = ""
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b StagingDeployRecord) int {
		if a.Live() != b.Live() {
			if a.Live() {
				return -1
			}
			return 1
		}
		return b.At.Compare(a.At)
	})
	return out
}

// event is Deps.Event: the engine's stagingEvent once bound (dropped
// before: no effect steps before the daemon runs).
func (b *stagingBound) event(eff staging.Effect, step, phase, text string) {
	if e := b.e.Load(); e != nil {
		e.stagingEvent(eff, step, phase, text)
	}
}

// stagingEvent writes one event of a deploy's effect into the registry,
// once per (effect, Seq, step, phase), and makes it the effect's record
// when its Seq is the newest the record has seen. A repeat writes no event
// but rewrites a record missing or behind its Seq: the write after the
// event's may have failed. An event the registry refused is not delivered
// again; the tick mends its record from the journal
// (tendStagingDeploys).
func (e *Engine) stagingEvent(eff staging.Effect, step, phase, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), stagingEventWait)
	defer cancel()
	subject := "pr:" + eff.Repo + "#" + strconv.Itoa(eff.PR.Number)
	text = stagingClean(text)
	level := "info"
	if phase == staging.PhaseFail {
		level = "warn"
	}
	msg := execx.Redact(fmt.Sprintf("staging deploy %s of %s#%d at %s to %s: %s %s", eff.ID, eff.Repo, eff.PR.Number,
		textx.ShortSHA(eff.PR.Head), eff.Env.Name, step, phase))
	if text != "" {
		msg += ": " + execx.Redact(text)
	}
	data, err := json.Marshal(map[string]any{"effect": eff.ID, "seq": eff.Seq, "env": eff.Env.Name, "state": eff.State,
		"why": stagingClean(eff.Why), "merge": eff.Merge, "run": eff.RunURL, "head": eff.PR.Head, "by": eff.By, "try": eff.Try})
	if err != nil {
		return
	}
	e.stagingEventMu.Lock()
	defer e.stagingEventMu.Unlock()
	added, err := e.st.AppendEventOnce(ctx, store.Event{Level: level, Subject: &subject, Kind: "staging.deploy", Step: &step, Phase: &phase,
		Message: msg, Data: json.RawMessage(execx.Redact(string(data)))}, fmt.Sprintf("%s/%d", eff.ID, eff.Seq))
	if err != nil {
		e.warnUnlessStopped(ctx, err, "staging: a deploy's event not recorded", "effect", eff.ID)
		return
	}
	if added {
		e.log.Log(ctx, execx.EventLevel(level), msg, "subject", subject, "kind", "staging.deploy")
	}
	k := KVStagingDeploy(string(eff.ID))
	if v, ok := e.getKV(ctx, k); ok {
		// An older write's event leaves the newer record; a repeat also
		// leaves one of its own Seq, which may hold a later event of it.
		if prev, ok := parseStagingDeployRecord(v); ok && (prev.Seq > eff.Seq || (!added && prev.Seq == eff.Seq)) {
			return
		}
	}
	e.putStagingDeploy(ctx, k, stagingDeployRecordOf(eff, step, phase, text, e.now()))
}

// stagingDeployRecordOf is eff's record at the event step, phase and text
// (its words made safe already), which came at now when eff says not when.
func stagingDeployRecordOf(eff staging.Effect, step, phase, text string, now time.Time) StagingDeployRecord {
	return StagingDeployRecord{Effect: string(eff.ID), Repo: eff.Repo, Number: eff.PR.Number, Head: eff.PR.Head, Env: eff.Env.Name, By: eff.By,
		Step: step, Phase: phase, State: eff.State, Why: stagingClean(eff.Why), Text: text, Merge: eff.Merge, RunURL: eff.RunURL,
		Seq: eff.Seq, At: cmp.Or(eff.Updated, now)}
}

// putStagingDeploy writes rec as the record k, redacted.
func (e *Engine) putStagingDeploy(ctx context.Context, k string, rec StagingDeployRecord) {
	if b, err := json.Marshal(rec); err == nil {
		e.setKV(ctx, k, execx.Redact(string(b)))
	}
}

// tendStagingDeploys keeps the deploys' records true to their journals
// (the tick's): Deps.Event cannot fail, so an event the registry refused
// (busy past stagingEventWait) is never delivered again, and its record
// stays behind its effect, a lost end reading live for ever. Each live
// record whose effect's journal (s.Effects, read once per repository: files
// under mirror_dir, no git) has a newer Seq takes the journal's state; a
// live one no journal holds, and an ended one, is dropped stagingDeployKeep
// after its last event.
func (e *Engine) tendStagingDeploys(ctx context.Context, s Staging) {
	values, err := e.st.KVValues(ctx, KVStagingDeploys)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "staging: read the deploys' records")
		return
	}
	now := e.now()
	journals := map[string][]staging.Effect{} // by lower-case repository
	for _, k := range slices.Sorted(maps.Keys(values)) {
		r, ok := parseStagingDeployRecord(values[k])
		if !ok {
			continue
		}
		old := now.Sub(r.At) > stagingDeployKeep
		if !r.Live() || KVStagingDeploy(r.Effect) != k {
			if old {
				e.delKV(ctx, k)
			}
			continue
		}
		repo := strings.ToLower(r.Repo)
		effs, read := journals[repo]
		if !read {
			effs = s.Effects(r.Repo)
			journals[repo] = effs
		}
		switch i := slices.IndexFunc(effs, func(f staging.Effect) bool { return string(f.ID) == r.Effect }); {
		case i >= 0 && effs[i].Seq > r.Seq:
			e.mendStagingDeploy(ctx, k, effs[i], now)
		case i < 0 && old:
			e.delKV(ctx, k)
		}
	}
}

// mendStagingDeploy rewrites the record k from its effect's journal (eff),
// unless an event made it as new meanwhile: its state, step and why, no
// event's words (the journal keeps none) nor phase.
func (e *Engine) mendStagingDeploy(ctx context.Context, k string, eff staging.Effect, now time.Time) {
	e.stagingEventMu.Lock()
	defer e.stagingEventMu.Unlock()
	v, ok := e.getKV(ctx, k)
	if !ok {
		return
	}
	if prev, ok := parseStagingDeployRecord(v); !ok || prev.Seq >= eff.Seq {
		return
	}
	e.log.Info("staging: a deploy's record caught up with its journal: an event of it was not recorded",
		"effect", eff.ID, "seq", eff.Seq, "state", eff.State)
	e.putStagingDeploy(ctx, k, stagingDeployRecordOf(eff, stagingClean(eff.Step), "", "", now))
}

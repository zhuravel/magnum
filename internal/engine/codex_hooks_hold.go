package engine

// Codex shows its hooks review ("Hooks need review") when a session starts
// while a hook of its home's hooks.json has no trust entry in config.toml,
// and a prompt typed into the review is lost. On 2026-10-23 a second wave
// ran from 09:06 to 09:55 UTC: magnum answered "Trust all" 13 times over 5
// sessions, Codex saved no trust, first prompts stalled with no rollout and
// no session id, and five rounds started in between; the toast came at
// 09:07 and the operator ran `codex` in a terminal at about 09:55. Four tools
// rewrite ~/.codex/hooks.json (herdr, Muxy, Bartender NotchBar, moshi), and
// any of their hooks can bring the review. So once a minute, with herdr's
// session hook checks, the daemon reads the trust of every hook of the
// hooks.json of each Codex home the codex kind's session roles read, and
// while one has none the codex kind is paused, which holds every new Codex
// session: rounds and jobs that need Codex wait, running sessions go on.
// magnum never writes hooks.json or config.toml.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// HooksReviewPauseReason is the codex kind's pause reason while a hook of the
// hooks.json of a Codex home its session roles read has no trust entry
// (checkCodexHooksTrust). The check renews it and ends it; it has no end of
// its own.
const HooksReviewPauseReason = "hooks_review"

// EventCodexHooksUntrusted (warn, subject tool:codex) says, once per
// hooks.json fingerprint, that the hooks without a trust entry of a Codex
// home hold new Codex sessions (data: hooks, the fingerprint; places;
// roles).
const EventCodexHooksUntrusted = "codex.hooks_untrusted"

const (
	// KVCodexHooksUntrusted holds the fingerprints (comma-separated) of the
	// hooks.json files that have hooks without a trust entry, which the
	// daemon told about once (EventCodexHooksUntrusted); absent when there
	// are none.
	KVCodexHooksUntrusted = "codex.hooks_untrusted"
	// KVCodexHooksOverruled holds the fingerprints of those `magnum resume
	// --tool codex` overruled (OverruleHooksHold): they hold no new session
	// while the file stays the same.
	KVCodexHooksOverruled = "codex.hooks_overruled"
)

// HooksOverruledNote is what a resume that overrules the hold means, as its
// answer says it.
const HooksOverruledNote = "new Codex sessions start although hooks.json has hooks without a trust entry, until it changes again"

// hooksHoldFor is how far ahead the hold's pause runs: the check renews it
// once a minute and ends it.
const hooksHoldFor = 10 * time.Minute

// The hold's toast.
const (
	hooksHoldTitle = "magnum: Codex hooks need review"
	hooksHoldBody  = "Codex hooks need review: run `codex` once and choose Trust all. New Codex sessions wait."
)

// checkCodexHooksTrust reads, for each Codex home of homes (those the codex
// kind's session roles read, agents.CodexSessionHomes, with users the roles
// that read each), which hooks of its hooks.json have no trust entry
// (agents.ReadCodexHooksTrust). A home with such a hook holds new Codex
// sessions (holdForHooks) unless `magnum resume --tool codex` overruled its
// hooks.json (KVCodexHooksOverruled, kept while the file is the same), with
// one EventCodexHooksUntrusted warning and one toast per hooks.json
// fingerprint (KVCodexHooksUntrusted). A file it cannot read holds nothing
// (`magnum doctor` and checkCodexHooksFile say so). Messages name
// ~/.codex/... or $CODEX_HOME/..., never a home from a pane env.
func (e *Engine) checkCodexHooksTrust(ctx context.Context, homes []string, users map[string][]string) {
	told, overruled := e.kvSet(ctx, KVCodexHooksUntrusted), e.kvSet(ctx, KVCodexHooksOverruled)
	untrusted, read := map[string]bool{}, map[string]bool{}
	complete := true // every home read: an overrule of a file that changed can go
	var held []string
	for _, home := range homes {
		if home == "" {
			continue
		}
		t, ok, err := agents.ReadCodexHooksTrust(home)
		if err != nil {
			complete = false
			e.log.Debug("codex hooks trust", "err", codexHomeErr(err, home))
			continue
		}
		if !ok {
			continue
		}
		read[t.Fingerprint] = true
		if len(t.Untrusted) == 0 {
			continue
		}
		untrusted[t.Fingerprint] = true
		if overruled[t.Fingerprint] {
			continue
		}
		hooksName := agents.CodexFileName(home, "hooks.json")
		held = append(held, t.UntrustedSentence(hooksName))
		if told[t.Fingerprint] {
			continue
		}
		places := make([]string, len(t.Untrusted))
		for i, h := range t.Untrusted {
			places[i] = h.Place
		}
		e.event(ctx, "warn", "tool:"+agents.KindCodex, EventCodexHooksUntrusted,
			fmt.Sprintf("Codex hooks need review: %s (%s), read by %s: Codex asks to review them when a session starts, and a prompt "+
				"typed into the review is lost, so run `codex` once and choose Trust all. New Codex sessions wait; "+
				"`magnum resume --tool codex` starts them anyway until %s changes again.",
				t.UntrustedSentence(hooksName), t.UntrustedPlaces(), strings.Join(users[home], ", "), hooksName),
			map[string]any{"hooks": t.Fingerprint, "places": places, "roles": users[home]})
		e.urgent("codex-hooks-trust:"+t.Fingerprint[:16], hooksHoldTitle, hooksHoldBody, codexHookToastWindow)
	}
	e.setKVSet(ctx, KVCodexHooksUntrusted, untrusted)
	if complete {
		maps.DeleteFunc(overruled, func(fp string, _ bool) bool { return !read[fp] })
		e.setKVSet(ctx, KVCodexHooksOverruled, overruled)
	}
	e.holdForHooks(ctx, held)
}

// holdForHooks keeps the codex kind paused for its hooks review
// (HooksReviewPauseReason, until hooksHoldFor from now, renewed at every
// check) while held names the hooks.json files whose hooks hold new
// sessions, and ends the pause it set once none does (one tool.resumed
// event). A pause of another reason is left alone: it says when it ends,
// and the hold comes at the check after it ended. A dry run records it.
func (e *Engine) holdForHooks(ctx context.Context, held []string) {
	kind := agents.KindCodex
	p, paused := e.toolPause(ctx, kind)
	ours := paused && p.Reason == HooksReviewPauseReason
	switch {
	case len(held) > 0 && (!paused || ours):
		detail := strings.Join(held, "; ") + ": run `codex` once and choose Trust all"
		if e.d.DryRun {
			e.rec.Record(ctx, "tool:"+kind, "pause", detail)
			return
		}
		e.setToolPause(ctx, kind, HooksReviewPauseReason, detail, e.now().Add(hooksHoldFor))
		if !ours {
			e.log.Info("codex paused: new Codex sessions wait for the hooks review", "detail", detail)
		}
	case len(held) == 0 && ours:
		if e.d.DryRun {
			e.rec.Record(ctx, "tool:"+kind, "resume", "no hooks.json holds new Codex sessions")
			return
		}
		e.clearToolPause(ctx, kind)
		e.event(ctx, "info", "tool:"+kind, "tool.resumed", fmt.Sprintf("%s pause (%s) ended: no hooks.json of the Codex roles has a hook "+
			"without a trust entry now", kind, HooksReviewPauseReason), nil)
	}
}

// overruleHooksHold is OverruleHooksHold for the daemon's `magnum resume
// --tool codex`, with one tool.resumed event when it overrules a hold.
func (e *Engine) overruleHooksHold(ctx context.Context) bool {
	ok, err := OverruleHooksHold(ctx, e.st)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "overrule the hooks hold")
		return false
	}
	if ok {
		e.event(ctx, "info", "tool:"+agents.KindCodex, "tool.resumed",
			fmt.Sprintf("%s hold (%s) overruled by magnum resume: %s", agents.KindCodex, HooksReviewPauseReason, HooksOverruledNote), nil)
	}
	return ok
}

// OverruleHooksHold lets the hooks.json files whose hooks without a trust
// entry the daemon told about (KVCodexHooksUntrusted) start new Codex
// sessions until they change (KVCodexHooksOverruled), for `magnum resume
// --tool codex` (or all): the daemon's, and the CLI's when no daemon runs.
// It reports whether it overruled a file not overruled yet.
func OverruleHooksHold(ctx context.Context, st *store.Store) (bool, error) {
	told, err := readKVSet(ctx, st, KVCodexHooksUntrusted)
	if err != nil {
		return false, err
	}
	overruled, err := readKVSet(ctx, st, KVCodexHooksOverruled)
	if err != nil {
		return false, err
	}
	added := false
	for fp := range told {
		if !overruled[fp] {
			overruled[fp], added = true, true
		}
	}
	if !added {
		return false, nil
	}
	return true, st.SetKV(ctx, KVCodexHooksOverruled, joinKVSet(overruled))
}

// readKVSet reads the comma-separated set at key (none when absent).
func readKVSet(ctx context.Context, st *store.Store, key string) (map[string]bool, error) {
	v, _, err := st.GetKV(ctx, key)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for s := range strings.SplitSeq(v, ",") {
		if s != "" {
			set[s] = true
		}
	}
	return set, nil
}

// joinKVSet is set as its kv value: sorted, comma-separated.
func joinKVSet(set map[string]bool) string {
	return strings.Join(slices.Sorted(maps.Keys(set)), ",")
}

// kvSet is readKVSet for the engine (none on a read error, logged).
func (e *Engine) kvSet(ctx context.Context, key string) map[string]bool {
	set, err := readKVSet(ctx, e.st, key)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "kv read", "key", key)
		return map[string]bool{}
	}
	return set
}

// setKVSet writes set at key, rewritten only when it changed; an empty set
// deletes the key.
func (e *Engine) setKVSet(ctx context.Context, key string, set map[string]bool) {
	v := joinKVSet(set)
	if prev, _ := e.getKV(ctx, key); prev == v {
		return
	}
	if v == "" {
		e.delKV(ctx, key)
		return
	}
	e.setKV(ctx, key, v)
}

// codexHomeErr is err's text with the Codex home home named as messages name
// it (agents.CodexFileName: ~/.codex or $CODEX_HOME), never its path, which
// may be a pane env value.
func codexHomeErr(err error, home string) string {
	return strings.ReplaceAll(err.Error(), home, strings.TrimSuffix(agents.CodexFileName(home, ""), "/"))
}

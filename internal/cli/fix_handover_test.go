package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/fixwork"
	"github.com/zhuravel/magnum/internal/store"
)

// setKVJSON stores v as JSON under key.
func (h *actHarness) setKVJSON(key string, v any) {
	h.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetKV(h.ctx, key, string(b)); err != nil {
		h.t.Fatal(err)
	}
}

// The morning list names every PR whose fix work keeps it from handing over
// to the factory, each record with what it is and the operator's action: a
// loop and its fixer's run end at the cycle boundary once the PR hands
// over; a held merge waits for P or a drop; a push record the PR's head
// left behind is his to drop; magnum's push waiting for its CI and a
// pending request end by themselves. A PR with none is not listed, nor one
// whose only record is a held fix, which goes with the PR for the factory
// to import: the list counts those apart.
func TestTheDrainedListNamesHisActionForEachRecord(t *testing.T) {
	h := newActHarness(t)
	h.d.Cfg.Factory.Fix = config.FactoryFixOn // a handover waits for the key while it is off: no PR hands over
	if code := h.cmd("fix", "--drained"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "none of magnum's fix work is in flight: every PR can hand over to the factory")

	at := h.now
	pr5 := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.setFixRow(pr5, engine.FixPhaseReviewing, 1)
	h.fixRunOf(pr5, 1, store.RunEnded)
	pr6 := h.seedPR("talkable/talkable", 6, store.PRNeedsAttention)
	h.setKVJSON(engine.KVPRHeldMerge(pr6.ID), fixwork.HeldMerge{Repo: "talkable/talkable", Number: 6, Merge: "d3d3d3d3", Head: pr6.HeadSHA,
		Base: "master", Tip: "c0c0c0c0", At: at})
	h.setKVJSON(fmt.Sprintf("pr.%d.fix_push", pr6.ID), fixwork.Push{Kind: fixwork.PushUnconfirmed, Run: "fix-1", Pushed: "b2b2b2b2", Failed: at})
	h.setKVJSON(fmt.Sprintf("pr.%d.pushed", pr6.ID), fixwork.CIPush{Head: "b2b2b2b2", At: at})
	h.setKVJSON(engine.KVPRFixHandover(pr6.ID), map[string]any{"at": at, "by": "magnum fix --handover"})
	pr7 := h.seedPR("talkable/talkable", 7, store.PRReviewed)
	h.setKVJSON(engine.KVPRFixHeld(pr7.ID), fixwork.HeldFix{At: at, Head: pr7.HeadSHA, By: "magnum fix"})
	if _, err := h.st.EnqueueRequest(h.ctx, engine.ReqFix, engine.FixPayload{PRTarget: engine.PRTarget{Repo: "talkable/talkable", Number: 7}}); err != nil {
		t.Fatal(err)
	}
	pr8 := h.seedPR("talkable/talkable", 8, store.PRReviewed)
	h.setKVJSON(engine.KVPRFixHeld(pr8.ID), fixwork.HeldFix{At: at, Head: pr8.HeadSHA, By: "magnum fix", Why: fixwork.HeldBudget})
	h.seedPR("talkable/talkable", 9, store.PRReviewed)

	h.out.Reset()
	if code := h.cmd("fix", "--drained"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out,
		"talkable#5  https://github.com/talkable/talkable/pull/5\n",
		"  a fix loop waiting for the re-review of its push (cycle 1 of 3)\n"+
			"    → `magnum fix --handover talkable/talkable#5` ends it at its cycle boundary, with no next cycle (`magnum abort talkable/talkable#5` ends it now)",
		"    → nothing: it ends with its loop",
		"talkable#6  https://github.com/talkable/talkable/pull/6  (hands over to the factory once this ends)\n",
		"  the push of b2b2b2b, kept for its late settle until "+fixwork.WaitClock(at.Add(24*time.Hour), h.now)+"\n",
		"    → `magnum fix --drop-push talkable/talkable#6` drops it (nothing to do once the PR's head is b2b2b2b: the poll settles it)",
		"  a merge of master at c0c0c0c held for your y\n"+
			"    → P pushes it (`magnum merge-parent talkable/talkable#6`), or `magnum merge-parent --drop talkable/talkable#6` drops it",
		"  magnum's push of b2b2b2b waits for its CI start\n    → nothing: it goes within 2m",
		"talkable#7  https://github.com/talkable/talkable/pull/7\n",
		"  a pending `magnum fix` request (1)\n    → nothing: the daemon answers it at its next tick",
		"2 held fixes hold no PR back: each goes with its PR, and the factory imports it\n",
	)
	if strings.Contains(out, "talkable#8") || strings.Contains(out, "talkable#9") || strings.Contains(out, "a fix held") {
		t.Errorf("a PR with no fix work in flight is listed:\n%s", out)
	}

	h.out.Reset()
	if code := h.cmd("fix", "--drained", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, h.errb.String())
	}
	var list []engine.UndrainedPR
	if err := json.Unmarshal(h.out.Bytes(), &list); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	var got []string
	for _, u := range list {
		for _, r := range u.Records {
			got = append(got, fmt.Sprintf("%d:%s", u.Number, r.Kind))
		}
	}
	want := []string{"5:loop", "5:run", "6:push", "6:held_merge", "6:pushed", "7:request"}
	if !slices.Equal(got, want) || !list[1].Handing || list[0].Handing {
		t.Errorf("records = %v (handing %v %v), want %v", got, list[0].Handing, list[1].Handing, want)
	}

	if code := h.cmd("fix", "5", "--drained"); code != 2 {
		t.Errorf("--drained with a PR: exit %d", code)
	}
}

// The morning list reads the registry alone: it says that the daemon's own
// reservations (a fix job winding down) are not in it, and names the check
// that counts them, the handover's dry run, which the daemon answers.
func TestTheDrainedListSaysTheDaemonsReservationsAreNotShown(t *testing.T) {
	h := newActHarness(t)
	const note = "the daemon's own reservations of PRs (a fix job winding down) are not shown here: " +
		"`magnum fix --handover <pr> --dry-run` asks the daemon, which counts them"
	if code := h.cmd("fix", "--drained"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), note)
	pr5 := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.setFixRow(pr5, engine.FixPhaseReviewing, 1)
	h.out.Reset()
	if code := h.cmd("fix", "--drained"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "talkable#5", note)
}

// --handover asks y/N on a terminal, then queues the daemon's request: one
// PR, every own PR, or back with one PR or all; --drop-push and
// merge-parent's --drop queue the morning list's drops. Each goes alone.
func TestFixHandoverAndTheDropsQueueTheirRequests(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.StdinTTY, h.d.StdoutTTY = true, true
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "the factory fixes talkable/talkable#5 from now on")
	}

	h.stdin("n\n")
	if code := h.cmd("fix", "--handover", "5"); code != 1 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "Hand the fixes of talkable#5 to the factory")
	actContains(t, h.errb.String(), "nothing queued")
	if reqs := h.requests(); len(reqs) != 0 {
		t.Fatalf("queued without a y: %+v", reqs)
	}
	h.stdin("y\n")
	h.out.Reset()
	if code := h.cmd("fix", "--handover", "5"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "the factory fixes talkable/talkable#5 from now on")
	for _, args := range [][]string{{"--handover", "all", "--yes"}, {"--handover", "back", "5", "--yes"}, {"--handover", "back", "all", "--yes"},
		{"5", "--drop-push", "--yes"}} {
		if code := h.cmd("fix", args...); code != 0 {
			t.Fatalf("fix %v: exit %d: %s", args, code, h.errb.String())
		}
	}
	if code := h.cmd("merge-parent", "5", "--drop", "--yes"); code != 0 {
		t.Fatalf("merge-parent --drop: exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 6 {
		t.Fatalf("requests = %+v", reqs)
	}
	one, all := actDecode[engine.HandoverPayload](t, reqs[0].Payload), actDecode[engine.HandoverPayload](t, reqs[1].Payload)
	back, backAll := actDecode[engine.HandoverPayload](t, reqs[2].Payload), actDecode[engine.HandoverPayload](t, reqs[3].Payload)
	switch {
	case reqs[0].Kind != engine.ReqFixHandover || one.Number != 5 || one.All || one.Back:
		t.Errorf("one = %s %+v", reqs[0].Kind, one)
	case !all.All || all.Back || all.Number != 0:
		t.Errorf("all = %+v", all)
	case !back.Back || back.All || back.Number != 5:
		t.Errorf("back = %+v", back)
	case !backAll.Back || !backAll.All:
		t.Errorf("back all = %+v", backAll)
	}
	push, drop := actDecode[engine.FixPayload](t, reqs[4].Payload), actDecode[engine.FixPayload](t, reqs[5].Payload)
	if reqs[4].Kind != engine.ReqFix || !push.DropPush || push.Number != 5 || reqs[5].Kind != engine.ReqFix || !drop.Drop || drop.Mode != engine.FixModeMerge {
		t.Errorf("drops = %s %+v / %s %+v", reqs[4].Kind, push, reqs[5].Kind, drop)
	}

	n := len(h.requests())
	for _, args := range [][]string{{"--handover", "all", "5"}, {"--handover", "back"}, {"--handover", "5", "--off"}, {"5", "--drop-push", "--wait"}} {
		h.errb.Reset()
		if code := h.cmd("fix", args...); code != 2 {
			t.Errorf("fix %v: exit %d, want 2", args, code)
		}
	}
	if code := h.cmd("merge-parent", "5", "--drop", "--wait"); code != 2 {
		t.Errorf("merge-parent --drop --wait: exit %d, want 2", code)
	}
	if got := len(h.requests()); got != n {
		t.Errorf("a usage error queued %d requests", got-n)
	}
}

// The morning list's drops say what they do: a drop unpins nothing, so a
// pinned PR's drop says nothing of an unpin, and merge-parent's drop with
// no terminal to ask on names a drop, never a merge, in its refusal.
func TestTheDropsSayWhatTheyDo(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242
	pr := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if err := h.st.UpdatePR(h.ctx, pr.ID, func(u *store.PRUpdate) { u.Set("pinned", true) }); err != nil {
		t.Fatal(err)
	}
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "dropped the record of the fixer's push of b2b2b2b on talkable/talkable#5: magnum settles it no more")
	}
	for _, args := range [][]string{{"fix", "5", "--drop-push", "--yes"}, {"merge-parent", "5", "--drop", "--yes"}} {
		h.out.Reset()
		h.errb.Reset()
		if code := h.cmd(args[0], args[1:]...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, h.errb.String())
		}
		if out := h.out.String() + h.errb.String(); strings.Contains(out, "unpins") {
			t.Errorf("%v says it unpins:\n%s", args, out)
		}
	}
	h.errb.Reset()
	if code := h.cmd("merge-parent", "5", "--drop"); code != 2 {
		t.Fatalf("merge-parent --drop without a terminal: exit %d", code)
	}
	if e := h.errb.String(); !strings.Contains(e, "a drop of the held merge asks y/N on a terminal") || !strings.Contains(e, "pass --yes to drop it without asking") ||
		strings.Contains(e, "to merge") {
		t.Errorf("refusal = %q", e)
	}
}

// The doctor names the PRs that wait to hand over to the factory and the
// reads of the factory's lease that failed open; with neither, one line
// counts magnum's fix work in flight.
func TestTheDoctorNamesTheHandoverAndTheLeasesThatFailedOpen(t *testing.T) {
	h := newActHarness(t)
	h.d.Cfg.Factory.Fix = config.FactoryFixOn // a handover waits for the key while it is off (TestTheDoctorSaysAHandoverWaitsForTheKey)
	d := doctorDeps{Store: h.st, Config: h.d.Cfg}
	cs := doctorFixHandover(h.ctx, d)
	if len(cs) != 1 || cs[0].Status != doctorPass || !strings.Contains(cs[0].Detail, "none of magnum's fix work is in flight") {
		t.Fatalf("checks = %+v", cs)
	}
	pr5 := h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.setFixRow(pr5, engine.FixPhaseFixing, 1)
	h.setKVJSON(engine.KVPRFixHandover(pr5.ID), map[string]any{"at": h.now, "by": "magnum fix --handover"})
	pr6 := h.seedPR("talkable/talkable", 6, store.PRReviewed)
	h.setKVJSON(engine.KVPRFixLeaseUnread(pr6.ID), engine.LeaseUnread{Since: h.now, Err: "database is locked", Open: true})
	h.setKVJSON(engine.KVPRFixHeld(pr6.ID), fixwork.HeldFix{At: h.now, Head: pr6.HeadSHA, By: "magnum fix"}) // holds nothing back
	cs = doctorFixHandover(h.ctx, d)
	if len(cs) != 2 {
		t.Fatalf("checks = %+v", cs)
	}
	if c := cs[0]; c.Status != doctorWarn || !strings.Contains(c.Detail, "1 PRs wait to hand over to the factory until magnum's fix work on them ends: talkable#5 (loop); "+
		"1 held fix holds no PR back, each going with its PR for the factory to import") || !strings.Contains(c.Fix, "magnum fix --drained") {
		t.Errorf("handover = %+v", c)
	}
	if c := cs[1]; c.Status != doctorWarn || !strings.Contains(c.Detail, "magnum reviews 1 PRs the factory fixes without its lease, which it cannot read (failed open): talkable#6 since") ||
		!strings.Contains(c.Detail, "(database is locked)") {
		t.Errorf("lease = %+v", c)
	}
}

// A handover written while [factory] fix was "on" waits for the key once
// it is off, and the doctor says so (the engine owner's ruling on R3): no
// PR flips meanwhile, and the line names the way out. With the key on,
// no such line.
func TestTheDoctorSaysAHandoverWaitsForTheKey(t *testing.T) {
	h := newActHarness(t)
	d := doctorDeps{Store: h.st, Config: h.d.Cfg}
	// The PASS line says that a handover is refused while the key is off,
	// as the handover's dry run does (C11).
	const refused = `; the handover itself is refused while [factory] fix is "off"`
	if cs := doctorFixHandover(h.ctx, d); len(cs) != 1 || cs[0].Status != doctorPass || !strings.HasSuffix(cs[0].Detail, refused) {
		t.Errorf("the PASS line with the key off = %+v", cs)
	}
	h.d.Cfg.Factory.Fix = config.FactoryFixOn
	if cs := doctorFixHandover(h.ctx, d); len(cs) != 1 || strings.Contains(cs[0].Detail, "refused") {
		t.Errorf("the PASS line with the key on = %+v", cs)
	}
	h.d.Cfg.Factory.Fix = config.FactoryFixOff
	h.setKVJSON(engine.KVFixHandoverAll, map[string]any{"at": h.now, "by": "magnum fix --handover all"})
	pr6 := h.seedPR("talkable/talkable", 6, store.PRReviewed)
	h.setKVJSON(engine.KVPRFixHandover(pr6.ID), map[string]any{"at": h.now, "by": "magnum fix --handover"})
	waits := func() (doctorCheck, bool) {
		for _, c := range doctorFixHandover(h.ctx, d) {
			if strings.Contains(c.Detail, "no PR flips to the factory") {
				return c, true
			}
		}
		return doctorCheck{}, false
	}
	c, ok := waits()
	if !ok || c.Status != doctorWarn || !strings.Contains(c.Detail, "`magnum fix --handover all` waits for [factory] fix = \"on\", and so does the handover of talkable#6") ||
		!strings.Contains(c.Detail, "no PR flips to the factory") ||
		!strings.Contains(c.Fix, "magnum fix --handover back all") {
		t.Errorf("the key's line = %+v, %v; checks %+v", c, ok, doctorFixHandover(h.ctx, d))
	}
	h.d.Cfg.Factory.Fix = config.FactoryFixOn
	if c, ok := waits(); ok {
		t.Errorf("with the key on: %+v", c)
	}
}

// A failed read whose source is magnum's own owner row (one this build
// cannot parse) sends the operator to magnum's registry, never to the
// factory's, which reads fine.
func TestTheDoctorNamesAnOwnerRowThatDoesNotParseAsMagnumsOwn(t *testing.T) {
	h := newActHarness(t)
	d := doctorDeps{Store: h.st, Config: h.d.Cfg}
	pr6 := h.seedPR("talkable/talkable", 6, store.PRReviewed)
	if err := h.st.SetKV(h.ctx, engine.KVPRFixLeaseUnread(pr6.ID),
		fmt.Sprintf(`{"since":%q,"err":"fix owner: unknown owner \"someone newer\"","source":"owner"}`, h.now.UTC().Format(time.RFC3339))); err != nil {
		t.Fatal(err)
	}
	pr7 := h.seedPR("talkable/talkable", 7, store.PRReviewed)
	if err := h.st.SetKV(h.ctx, engine.KVPRFixLeaseUnread(pr7.ID),
		fmt.Sprintf(`{"since":%q,"err":"fix owner: unknown owner \"someone newer\"","open":true,"source":"owner"}`, h.now.UTC().Format(time.RFC3339))); err != nil {
		t.Fatal(err)
	}
	cs := doctorFixHandover(h.ctx, d)
	if len(cs) != 2 {
		t.Fatalf("checks = %+v", cs)
	}
	c := cs[1]
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "talkable#6") || !strings.Contains(c.Detail, "its owner row in magnum's registry") ||
		!strings.Contains(c.Fix, "pr.*.fix_owner") || strings.Contains(c.Fix, "the factory's registry cannot be read") {
		t.Errorf("owner row = %+v", c)
	}
	// The headlines name the owner row too, never the factory's lease (C3).
	if strings.Contains(c.Detail, "lease") || !strings.Contains(c.Detail, "magnum reviews 1 PRs without their owner row, which it cannot read (failed open): talkable#7") ||
		!strings.Contains(c.Detail, "1 PRs' rounds wait for their owner row, which cannot be read: talkable#6") || strings.Contains(c.Name, "lease") {
		t.Errorf("owner row headlines = %+v", c)
	}
	// An owner row magnum's registry cannot read is named as that
	// registry's failed read, and its fix sends the operator there (R2).
	pr8 := h.seedPR("talkable/talkable", 8, store.PRReviewed)
	h.setKVJSON(engine.KVPRFixLeaseUnread(pr8.ID), engine.LeaseUnread{Since: h.now, Err: "database disk image is malformed", Open: true,
		Source: engine.LeaseFromRegistry})
	if cs = doctorFixHandover(h.ctx, d); len(cs) != 2 {
		t.Fatalf("checks = %+v", cs)
	}
	if c := cs[1]; !strings.Contains(c.Detail, "magnum reviews 2 PRs without their owner row") || strings.Contains(c.Detail, "lease") ||
		!strings.Contains(c.Detail, "talkable#8 since") || !strings.Contains(c.Detail, "(magnum's registry cannot read it: database disk image is malformed)") ||
		!strings.Contains(c.Fix, "magnum's registry cannot read an owner row (pr.*.fix_owner)") || !strings.Contains(c.Fix, "this build cannot parse") {
		t.Errorf("registry = %+v", c)
	}
}

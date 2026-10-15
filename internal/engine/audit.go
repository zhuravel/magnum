package engine

// Audit passes (DECISIONS "Every fifth re-review audits what no round
// probed"): the PR's evidence map (evidence.go) says which areas no round
// read or probed and which changed since a round did, and a re-review reads
// the delta alone, so those areas stay unread round after round; a full
// pass reads the whole PR again, at the cost of a first review. So the Nth
// automatic re-review of a PR ([pipeline] audit_every), counted since its
// first review, its last full pass or its last audit pass, runs as an
// audit pass while the map holds unknown or stale entries: every session
// starts fresh, the round runs as a recovery whose reviewers re-review the
// delta (pipeline.RoundInput.ColdJudge), and the judge first probes at most
// three of those areas, the riskiest first (an irreversible write,
// authorization, money, an optional branch, a pre-pass lead), and one
// earlier probe again, the oldest on a file the pushes touched since
// (pipeline.RoundInput.Audit), each through its real entry point with a
// control, then re-reviews the delta. An unknown area is a lead, never a
// finding. The count reads the runs table as the full pass's does
// (fullpass.go), and the audit pass counts toward the full pass's; a full
// pass due on the same round wins. A delta check, a re-review of the same
// head, a reply round, a CI check and a round someone asked for never
// become one.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRAuditPass holds the PR's last audit pass that posted (a
// fullPassRecord as JSON): the re-reviews of later rounds count toward the
// next one.
func kvPRAuditPass(prID int64) string { return fmt.Sprintf("pr.%d.audit_pass", prID) }

// kvPRAuditRound is "1" while the PR's round in flight is an audit pass (a
// continue of a paused one goes on as one, and records it when it posts);
// the next round's start resets it, a posted audit pass deletes it.
func kvPRAuditRound(prID int64) string { return fmt.Sprintf("pr.%d.audit_round", prID) }

// auditReReviewLabel is how the round's start event names an audit pass.
const auditReReviewLabel = "re-review (audit pass)"

// maxAuditAreas bounds the unknown or stale areas an audit pass probes,
// the probe run again aside.
const maxAuditAreas = 3

// auditRisks rank an area by the words of its path, symbol and behaviour
// (lower case, substrings), the first that matches: an irreversible write,
// authorization, money, an optional branch. A pre-pass lead comes next.
var auditRisks = []struct {
	why   string
	words []string
}{
	{"irreversible write", []string{"delete", "destroy", "drop", "remove", "purge", "truncate", "overwrite", "migrat", "update", "insert",
		"write", "save", "restore", "import", "send", "deliver", "publish"}},
	{"authorization", []string{"auth", "permission", "access", "admin", "role", "token", "secret", "session", "login", "policy", "owner",
		"csrf", "signature", "scope"}},
	{"money", []string{"price", "amount", "payment", "pay", "charge", "refund", "billing", "invoice", "currency", "money", "reward",
		"coupon", "discount", "credit", "balance", "cost", "fee", "tax", "incentive"}},
	{"optional branch", []string{"optional", "nil", "null", "empty", "missing", "fallback", "default", "absent", "blank", "unless", "else",
		"without"}},
}

// prepassLead is why an area on a file of the pre-pass's lists ranks.
const prepassLead = "pre-pass lead"

// auditDue reports why job, a re-review the dispatcher plans, runs as an
// audit pass ("" = it does not): audit_every is set, no full pass is due
// (job.fullPass), the head moved since the review, the PR's evidence map
// holds unknown or stale entries, and this automatic re-review is the Nth
// since the PR's first review, its last full pass or its last audit pass
// (auditBase). A forced or requested round, a post-merge review, a delta
// check, a re-review of the same head, a reply round and a CI check are
// none.
func (e *Engine) auditDue(ctx context.Context, job *roundJob) string {
	pr := job.pr
	every := e.cfg.Pipeline.AuditEvery
	switch {
	case every <= 0, job.kind != pipeline.KindRereview, job.fullPass != "", postMerge(pr), pr.Forced, job.requested, job.judgeOnly(),
		pr.HeadSHA == deref(pr.ReviewedSHA):
		return ""
	}
	after, since, _ := e.auditBase(ctx, pr.ID)
	n := e.rereviewsAfter(ctx, pr.ID, after)
	if n+1 < every {
		return ""
	}
	m, ok, err := ReadEvidenceMap(pipeline.EvidenceMapPath(e.d.Layout, job.repo.Owner, job.repo.Name, pr.Number))
	if err != nil || !ok {
		return ""
	}
	c := m.Counts()
	if c.Unknown+c.Stale == 0 {
		return ""
	}
	return fmt.Sprintf("re-review %d since %s (audit_every %d), with %d unknown and %d stale",
		n+1, since, every, c.Unknown, c.Stale)
}

// auditBase is the round after which the audit count starts: the PR's
// last audit pass or full pass that posted, whichever came later, with its
// words and the head it reviewed; 0, "the first review" and "" when
// neither did.
func (e *Engine) auditBase(ctx context.Context, prID int64) (round int, since, head string) {
	full, _ := e.lastFullPass(ctx, prID)
	audit, _ := e.lastAudit(ctx, prID)
	switch {
	case audit.Round > full.Round:
		return audit.Round, fmt.Sprintf("the audit pass of round %d", audit.Round), audit.Target
	case full.Round > 0:
		return full.Round, fmt.Sprintf("the full pass of round %d", full.Round), full.Target
	}
	return 0, "the first review", ""
}

// rereviewsAfter counts PR prID's re-reviews numbered after round, as
// rereviewsSinceFullPass does after the last full pass: its rounds of kind
// rereview or recovery whose judge posted a review on a head other than
// the one reviewed before them. A read error counts none.
func (e *Engine) rereviewsAfter(ctx context.Context, prID int64, round int) int {
	runs, err := e.st.RunsByPR(ctx, prID)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "audit pass: runs", "pr", prID)
		return 0
	}
	rounds := map[int]bool{}
	for _, r := range runs {
		prev := deref(r.PrevReviewedSHA)
		switch {
		case r.Round <= round, r.Kind != store.RunRereview && r.Kind != store.RunRecovery,
			deref(r.ReviewID) == 0, prev == "", prev == r.TargetSHA:
			continue
		}
		rounds[r.Round] = true
	}
	return len(rounds)
}

// lastAudit is the PR's last audit pass that posted (kvPRAuditPass); ok is
// false when none did, or its record cannot be read.
func (e *Engine) lastAudit(ctx context.Context, prID int64) (fullPassRecord, bool) {
	v, ok := e.getKV(ctx, kvPRAuditPass(prID))
	var rec fullPassRecord
	if !ok || json.Unmarshal([]byte(v), &rec) != nil {
		return fullPassRecord{}, false
	}
	return rec, true
}

// planAudit settles whether the round prepare sets up is an audit pass: a
// re-review the dispatcher made one (roundJob.audit) gets its areas
// (auditAreas; none left: an ordinary re-review), and a continue of a
// paused audit pass goes on as one.
func (e *Engine) planAudit(ctx context.Context, job *roundJob, rs *roundSetup) {
	switch {
	case job.kind == kindContinue:
		v, _ := e.getKV(ctx, kvPRAuditRound(job.pr.ID))
		rs.auditPass = v == "1"
	case job.kind == pipeline.KindRereview && job.audit != "" && !rs.judgeOnly() && !rs.fullPass:
		rs.audit = e.auditAreas(ctx, job, rs.target)
		rs.auditPass = len(rs.audit) > 0
	}
}

// noteAuditRound records whether the PR's round runs an audit pass
// (kvPRAuditRound).
func (e *Engine) noteAuditRound(ctx context.Context, prID int64, on bool) {
	if on {
		e.setKV(ctx, kvPRAuditRound(prID), "1")
		return
	}
	e.delKV(ctx, kvPRAuditRound(prID))
}

// settleAudit records an audit pass that posted (kvPRAuditPass: its judge
// run, round and target, from which the next count starts) and ends its
// mark.
func (e *Engine) settleAudit(ctx context.Context, prID int64, res pipeline.RoundResult, target string) {
	if v, _ := e.getKV(ctx, kvPRAuditRound(prID)); v != "1" {
		return
	}
	if b, err := json.Marshal(fullPassRecord{Run: res.JudgeRunID, Round: res.Round, Target: target, At: e.now().UTC()}); err == nil {
		e.setKV(ctx, kvPRAuditPass(prID), string(b))
	}
	e.delKV(ctx, kvPRAuditRound(prID))
}

// auditAreas are the areas an audit pass of job on target probes
// (pickAudit), from the PR's evidence map, the pre-pass of its reviewed
// head (prepassLeads) and the files the pushes touched since the last
// audit or full pass, else since the map's oldest head (one
// Git.ChangedBetween; none when git cannot tell, so no probe is run
// again).
func (e *Engine) auditAreas(ctx context.Context, job *roundJob, target string) []pipeline.AuditArea {
	pr := job.pr
	m, ok, err := ReadEvidenceMap(pipeline.EvidenceMapPath(e.d.Layout, job.repo.Owner, job.repo.Name, pr.Number))
	if err != nil || !ok {
		return nil
	}
	_, _, since := e.auditBase(ctx, pr.ID)
	if since == "" {
		since = oldestHead(m)
	}
	var touched []string
	if since != "" && since != target {
		if touched, err = e.d.Git.ChangedBetween(ctx, job.slot.Path, since, target); err != nil {
			e.log.Info("audit pass: no diff since the base; no probe runs again", "pr", pr.ID,
				"from", textx.ShortSHA(since), "to", textx.ShortSHA(target), "err", err)
		}
	}
	leads := prepassLeads(e.d.Layout.ReviewDir(job.repo.Owner, job.repo.Name, pr.Number, deref(pr.ReviewedSHA)))
	return pickAudit(m, leads, touched)
}

// oldestHead is the head of m's oldest entry (its lowest round), "" for
// none.
func oldestHead(m EvidenceMap) string {
	head, round := "", 0
	for _, x := range m.Entries {
		if x.Head != "" && (head == "" || x.Round < round) {
			head, round = x.Head, x.Round
		}
	}
	return head
}

// prepassLeads are the paths the pre-pass in dir (prepass.json) names: the
// changed source files without a test change, the added files with a
// sibling, the files a removed definition left and those that still name
// it. None when there is no such file.
func prepassLeads(dir string) map[string]bool {
	b, err := os.ReadFile(filepath.Join(dir, pipeline.PrepassFile))
	var p pipeline.Prepass
	if err != nil || json.Unmarshal(b, &p) != nil {
		return nil
	}
	leads := map[string]bool{}
	for _, f := range p.TestGaps {
		leads[f] = true
	}
	for _, s := range p.Siblings {
		leads[s.File] = true
	}
	for _, r := range p.RemovedSymbols {
		leads[r.File] = true
		for _, u := range r.Uses {
			path, _, _ := strings.Cut(u, ":")
			leads[path] = true
		}
	}
	return leads
}

// auditRisk ranks x by auditRisks, then as a pre-pass lead, and says why;
// len(auditRisks)+1 and "" when nothing makes it risky.
func auditRisk(x EvidenceMapEntry, leads map[string]bool) (int, string) {
	text := strings.ToLower(x.Path + " " + x.Symbol + " " + x.Behaviour)
	for i, r := range auditRisks {
		if slices.ContainsFunc(r.words, func(w string) bool { return strings.Contains(text, w) }) {
			return i, r.why
		}
	}
	if leads[x.Path] {
		return len(auditRisks), prepassLead
	}
	return len(auditRisks) + 1, ""
}

// pickAudit chooses an audit pass's areas from m: at most maxAuditAreas of
// its unknown or stale entries, the riskiest first (auditRisk), an unknown
// one before a stale one, then the oldest; then the oldest probe (lowest
// round) on a file of touched, to run again ("rotation").
func pickAudit(m EvidenceMap, leads map[string]bool, touched []string) []pipeline.AuditArea {
	type candidate struct {
		x    EvidenceMapEntry
		rank int
		why  string
	}
	var cs []candidate
	for _, x := range m.Entries {
		if x.State == pipeline.EvidenceUnknown || x.State == pipeline.EvidenceStale {
			rank, why := auditRisk(x, leads)
			cs = append(cs, candidate{x, rank, why})
		}
	}
	stale := func(c candidate) bool { return c.x.State == pipeline.EvidenceStale }
	slices.SortStableFunc(cs, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), compareBool(stale(a), stale(b)), cmp.Compare(a.x.Round, b.x.Round),
			strings.Compare(a.x.Path, b.x.Path), strings.Compare(a.x.Behaviour, b.x.Behaviour))
	})
	var out []pipeline.AuditArea
	for _, c := range cs[:min(len(cs), maxAuditAreas)] {
		out = append(out, auditArea(c.x, c.why))
	}
	var again *EvidenceMapEntry
	for i, x := range m.Entries {
		if x.State == pipeline.EvidenceProbed && slices.Contains(touched, x.Path) && (again == nil || x.Round < again.Round) {
			again = &m.Entries[i]
		}
	}
	if again != nil {
		out = append(out, auditArea(*again, "rotation"))
	}
	return out
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

func auditArea(x EvidenceMapEntry, why string) pipeline.AuditArea {
	return pipeline.AuditArea{EvidenceEntry: x.EvidenceEntry, Was: x.Was, Head: x.Head, Round: x.Round, Why: why}
}

// auditStarted records the audit pass's start: a round.audit_pass event
// naming why and how many areas the judge probes first.
func (e *Engine) auditStarted(ctx context.Context, job *roundJob, rs *roundSetup) {
	e.event(ctx, "info", prSubject(job.repo, job.pr.Number), "round.audit_pass",
		fmt.Sprintf("audit pass: %s: every session starts fresh, and the judge first probes %s of the evidence map",
			job.audit, textx.Count(len(rs.audit), "area", "areas")),
		map[string]any{"reason": job.audit, "audit_every": e.cfg.Pipeline.AuditEvery, "areas": len(rs.audit)})
}

package engine

// The factory part of the status comment: on a PR a factory task published,
// the factory daemon keeps a status file (internal/prstatus: counts, enums,
// times, the staging environments' slugs and the Jira key, never free
// text), and the comment renders it below magnum's lines, as a short
// heading, the table of the factory's steps (a Staging row per staging env)
// and a Now line. Every word it posts comes
// from the word maps below, a number or a time; the strings the file may
// carry, the envs' slugs and the Jira key, are checked again before they
// post. A row with nothing to say reads "none". The comment's last edit
// (a close, a merge, a draft) says the end in the Now line while the file
// still says the PR is open: the factory writes its own end after
// magnum's last edit, which nothing follows.
//
// Change detection: writeStatus reads the file each time it looks at a
// comment it may write (no more often than the tick already looks at the
// PR) and compares the sum of the part it renders with the one the comment
// last carried (kvPRFactoryStatus). The part changes with the file's
// content (its digest), when its "updated" time gains its day at the next
// UTC day (once per status) and at the comment's end; a new sum, or a file
// gone or refused after one, edits the comment as a change of magnum's
// lines does, under the same spacing and only while the comment goes on (a
// comment that ended stays as its last edit wrote it); the same sum edits
// nothing, whatever the factory rewrites.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// factoryUnknown is what the factory part says of a word outside its map
// (Read refuses a file holding one, so only a status built otherwise can).
const factoryUnknown = "not known"

// factoryNone is the row of a step with nothing to say: no local review, no
// fix loop yet, no staging deploy.
const factoryNone = "none"

// The word maps of the factory part: every enum word of the contract to
// the comment's fixed words (TestEveryFactoryEnumWordHasItsWords holds them
// to the contract's words).
var (
	factoryProofWords = map[prstatus.Proof]string{
		prstatus.ProofNone:       "no proof",
		prstatus.ProofRed:        "red on the base, not green at the head",
		prstatus.ProofRedToGreen: "red on the base, green at the head",
		prstatus.ProofGreen:      "green at the head",
	}
	factoryVerdictWords = map[prstatus.Verdict]string{
		prstatus.VerdictNone:     "no verdict",
		prstatus.VerdictClean:    "clean",
		prstatus.VerdictFindings: "findings left",
		prstatus.VerdictFailed:   "did not finish",
	}
	factoryFixWords = map[prstatus.FixState]string{
		prstatus.FixNone:     factoryNone,
		prstatus.FixRunning:  "running",
		prstatus.FixNeedsYou: "left to the operator",
		prstatus.FixDone:     "done",
		prstatus.FixOff:      "off",
	}
	factoryStagingWords = map[prstatus.StagingState]string{
		prstatus.StagingNone:      factoryNone,
		prstatus.StagingDeploying: "deploying",
		prstatus.StagingDeployed:  "deployed",
		prstatus.StagingFailed:    "failed",
	}
	factoryWhoWords = map[prstatus.Who]string{
		prstatus.WhoYou:       "waits on the operator",
		prstatus.WhoMagnum:    "waits on Magnum",
		prstatus.WhoReviewers: "waits on the reviewers",
		prstatus.WhoCI:        "waits on CI",
		prstatus.WhoFactory:   "waits on the factory",
		prstatus.WhoNone:      "waits on nobody",
	}
	factoryJiraWords = map[prstatus.JiraStatus]string{
		prstatus.JiraToDo:          "to do",
		prstatus.JiraInDevelopment: "in development",
		prstatus.JiraInCodeReview:  "in code review",
		prstatus.JiraDone:          "done",
		prstatus.JiraOther:         "another status",
		prstatus.JiraUnknown:       "status not known",
	}
	// factoryStageWords: the published PR's stage says nothing ("").
	factoryStageWords = map[prstatus.Stage]string{
		prstatus.StagePlan:      "planning",
		prstatus.StageCode:      "coding",
		prstatus.StageReview:    "in local review",
		prstatus.StageReady:     "ready to publish",
		prstatus.StagePublished: "",
		prstatus.StageDone:      "merged",
		prstatus.StageStopped:   "the task stopped",
		prstatus.StageFailed:    "the task failed",
	}
)

// factoryWord is m's words for w, factoryUnknown for a word m lacks.
func factoryWord[K ~string](m map[K]string, w K) string {
	if s, ok := m[w]; ok {
		return s
	}
	return factoryUnknown
}

// The words of the Now line on the comment's last edit while the file
// says the PR is open (factoryEnd).
const (
	factoryEndMerged = "merged"
	factoryEndClosed = "closed"
	factoryEndDraft  = "a draft"
)

// factoryEnd is the end the factory part's Now line says on v, pr's
// comment's last edit, in magnum's words: merged or closed as GitHub has
// it, else (a PR turned into a draft) a draft; "" while the comment goes on.
func factoryEnd(pr store.PR, v statusView) string {
	switch {
	case !v.ends:
		return ""
	case postMerge(pr):
		return factoryEndMerged
	case pr.GHState == store.GHClosed:
		return factoryEndClosed
	}
	return factoryEndDraft
}

// factoryEnded reports whether stage is the task's own end, which the Now
// line says as the file does.
func factoryEnded(stage prstatus.Stage) bool {
	return stage == prstatus.StageDone || stage == prstatus.StageStopped || stage == prstatus.StageFailed
}

// factoryText is the factory part of a status comment for s at now: the
// heading (with the Jira issue when s has one), the table of the steps and
// the Now line, whose time has its day once it is not now's UTC day
// (statusClock), as the comment's other times. On the comment's last edit
// (end, factoryEnd's words; "" while it goes on) the Now line is the end
// alone unless s says the task's own end: the file's time is from before
// the end, and the header has the end's.
func factoryText(s prstatus.Status, end string, now time.Time) string {
	head := "**Magnum factory**"
	if key := s.Jira.Key; factoryKeyOK(key) {
		head += " · Jira " + key + ": " + factoryWord(factoryJiraWords, s.Jira.Status)
	}
	lines := []string{head, "", "| Step | State |", "|---|---|"}
	rows := [][2]string{
		{"Plan", factoryPlan(s.Plan)},
		{"Code", factoryCode(s.Code)},
		{"Local review", factoryReview(s.LocalReview)},
		{"Published", statusDay(s.PublishedAt)},
		{"Fix loop", factoryFix(s.FixLoop)},
	}
	for _, r := range append(rows, factoryStagingRows(s)...) {
		lines = append(lines, "| "+r[0]+" | "+r[1]+" |")
	}
	nowLine := "Now: " + factoryNowWords(s.Now) + " · updated " + statusClock(s.UpdatedAt, now)
	if end != "" && !factoryEnded(s.Now.Stage) {
		nowLine = "Now: " + end
	}
	return strings.Join(append(lines, "", nowLine), "\n")
}

// factoryPlan is the Plan row: the questions the task asked and the ones
// answered.
func factoryPlan(p prstatus.Plan) string {
	if p.QuestionsAsked == 0 && p.QuestionsAnswered == 0 {
		return "no questions"
	}
	return textx.Count(p.QuestionsAsked, "question", "questions") + ", " + strconv.Itoa(p.QuestionsAnswered) + " answered"
}

// factoryCode is the Code row: the commits and spec files counted (a count
// of 0 is left out: a ready card an older build drew counts no commits),
// then the proof.
func factoryCode(c prstatus.Code) string {
	var counts []string
	if c.Commits > 0 {
		counts = append(counts, textx.Count(c.Commits, "commit", "commits"))
	}
	if c.Specs > 0 {
		counts = append(counts, textx.Count(c.Specs, "spec", "specs"))
	}
	proof := factoryWord(factoryProofWords, c.Proof)
	switch {
	case len(counts) > 0:
		return strings.Join(counts, ", ") + ": " + proof
	case c.Proof == prstatus.ProofNone:
		return factoryNone
	}
	return proof
}

// factoryReview is the Local review row: its rounds, its verdict and the
// notes it left open.
func factoryReview(r prstatus.LocalReview) string {
	if r.Rounds == 0 && r.Verdict == prstatus.VerdictNone && r.NotesOpen == 0 {
		return factoryNone
	}
	var parts []string
	if r.Rounds > 0 {
		parts = append(parts, textx.Count(r.Rounds, "round", "rounds"))
	}
	parts = append(parts, factoryWord(factoryVerdictWords, r.Verdict))
	if r.NotesOpen > 0 {
		parts = append(parts, textx.Count(r.NotesOpen, "note left open", "notes left open"))
	}
	return strings.Join(parts, ", ")
}

// factoryFix is the Fix loop row: its state, then its cycle, pushes and
// threads answered (each left out at 0); none before a first loop.
func factoryFix(f prstatus.FixLoop) string {
	if f.State == prstatus.FixNone {
		return factoryNone
	}
	parts := []string{factoryWord(factoryFixWords, f.State)}
	if f.Cycle > 0 {
		parts = append(parts, "cycle "+strconv.Itoa(f.Cycle))
	}
	if f.Pushes > 0 {
		parts = append(parts, textx.Count(f.Pushes, "push", "pushes"))
	}
	if f.ThreadsAnswered > 0 {
		parts = append(parts, textx.Count(f.ThreadsAnswered, "thread answered", "threads answered"))
	}
	return strings.Join(parts, ", ")
}

// factoryStagingRows are the Staging rows of s: one per entry of Stagings,
// in their order (the config's), "Staging · <env>" with that env's deploy
// state (the env's name only when it is a slug, prstatus.StagingEnv,
// checked again here; "Staging" alone otherwise), or, for a status without
// Stagings (a writer from before them, a repository without staging envs),
// the one row of its Staging field (factoryStaging).
func factoryStagingRows(s prstatus.Status) [][2]string {
	if len(s.Stagings) == 0 {
		return [][2]string{{"Staging", factoryStaging(s.Staging)}}
	}
	rows := make([][2]string, 0, len(s.Stagings))
	for _, st := range s.Stagings {
		step := "Staging"
		if env := prstatus.StagingEnv(st.Env); env != "" {
			step += " · " + env
		}
		rows = append(rows, [2]string{step, factoryWord(factoryStagingWords, st.State)})
	}
	return rows
}

// factoryStaging is the Staging row of a status without Stagings: the
// deploy's state, with the env's name when it is a slug
// (prstatus.StagingEnv, checked again here).
func factoryStaging(st prstatus.Staging) string {
	words := factoryWord(factoryStagingWords, st.State)
	env := prstatus.StagingEnv(st.Env)
	switch {
	case env == "", st.State == prstatus.StagingNone, words == factoryUnknown:
		return words
	case st.State == prstatus.StagingFailed:
		return words + " on `" + env + "`"
	}
	return words + " to `" + env + "`"
}

// factoryNowWords is who the PR waits on and the task's stage when it is
// not the published PR's; an end that waits on nobody is the end alone.
func factoryNowWords(n prstatus.Now) string {
	who, stage := factoryWord(factoryWhoWords, n.Who), factoryWord(factoryStageWords, n.Stage)
	switch {
	case stage == "":
		return who
	case n.Who == prstatus.WhoNone:
		return stage
	}
	return who + ", " + stage
}

// factoryKeyOK reports whether key may post as the Jira key: what Read
// takes (ABC-123) reads as one, and nothing but capitals, digits, "_" and
// "-" ever posts, whatever the status holds.
func factoryKeyOK(key string) bool {
	if key == "" || len(key) > 45 {
		return false
	}
	for _, r := range key {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// kvPRFactoryStatus holds the sum (statusSum) of the factory part the PR's
// status comment last carried, as rendered; absent: none.
func kvPRFactoryStatus(prID int64) string { return fmt.Sprintf("pr.%d.factory_status", prID) }

// factoryPart is pr's factory part at now (end: factoryEnd's words) and its
// sum, both "" when there is none (no file, or one Read refuses, which
// counts as missing). It reads the file, read-only, each time writeStatus
// looks at a comment that may be written, and logs a refusal once while it
// stays the same (a refusal names the reason and the field, never the
// file's content).
func (e *Engine) factoryPart(repo store.Repo, pr store.PR, end string, now time.Time) (text, sum string) {
	key := fmt.Sprintf("factory_status:%d", pr.ID)
	path, err := prstatus.Path(e.d.Layout, repo.Owner, repo.Name, pr.Number)
	var s prstatus.Status
	ok := false
	if err == nil {
		s, ok, err = prstatus.Read(path)
	}
	if err != nil {
		if msg := textx.FirstLine(execx.Redact(err.Error())); e.changed(key, msg) {
			e.log.Warn("a factory PR's status file is refused: its status comment shows no factory part", "pr", pr.ID, "err", msg)
		}
		return "", ""
	}
	delete(e.lastSeen, key) // a refusal after this read is new
	if !ok {
		return "", ""
	}
	text = factoryText(s, end, now)
	return text, statusSum(text)
}

// saveFactoryShown records sum as the factory part's the PR's comment now
// carries, when it is not the one recorded (was).
func (e *Engine) saveFactoryShown(ctx context.Context, prID int64, was, sum string) {
	switch sum {
	case was:
	case "":
		e.delKV(ctx, kvPRFactoryStatus(prID))
	default:
		e.setKV(ctx, kvPRFactoryStatus(prID), sum)
	}
}

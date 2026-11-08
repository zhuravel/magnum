package engine

// The factory part of the status comment: on a PR a factory task published,
// the factory daemon keeps a status file (internal/prstatus: counts, enums,
// times, the staging environment's slug and the Jira key, never free text),
// and the comment renders it below magnum's lines, as a short heading, the
// table of the factory's steps and a Now line. Every word it posts comes
// from the word maps below, a number or a time; the two strings the file
// may carry, the env's slug and the Jira key, are checked again before they
// post. A row with nothing to say reads "none".

import (
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/prstatus"
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

// factoryText is the factory part of a status comment for s at now: the
// heading (with the Jira issue when s has one), the table of the steps and
// the Now line, whose time has its day once it is not now's UTC day
// (statusClock), as the comment's other times.
func factoryText(s prstatus.Status, now time.Time) string {
	head := "**Magnum factory**"
	if key := s.Jira.Key; factoryKeyOK(key) {
		head += " · Jira " + key + ": " + factoryWord(factoryJiraWords, s.Jira.Status)
	}
	lines := []string{head, "", "| Step | State |", "|---|---|"}
	for _, r := range [][2]string{
		{"Plan", factoryPlan(s.Plan)},
		{"Code", factoryCode(s.Code)},
		{"Local review", factoryReview(s.LocalReview)},
		{"Published", statusDay(s.PublishedAt)},
		{"Fix loop", factoryFix(s.FixLoop)},
		{"Staging", factoryStaging(s.Staging)},
	} {
		lines = append(lines, "| "+r[0]+" | "+r[1]+" |")
	}
	return strings.Join(append(lines, "", "Now: "+factoryNowWords(s.Now)+" · updated "+statusClock(s.UpdatedAt, now)), "\n")
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

// factoryStaging is the Staging row: the deploy's state, with the env's
// name when it is a slug (prstatus.StagingEnv, checked again here).
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

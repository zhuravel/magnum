package factory

// The new-task screen's preview (ReqPreview): what a start of the text
// would make, read by the daemon, which makes nothing.

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// preview is the new-task screen's preview of text: the ticket the daemon
// read for a key (or the reason it cannot: Err), the guidance the start
// would send every agent with it, the kind and why, the title of a prompt,
// the branch, when the task would start, and its first agents. It makes
// nothing.
func (e *Engine) preview(ctx context.Context, text string) (tui.NewTaskForm, error) {
	f := tui.NewTaskForm{Text: text}
	text = strings.TrimSpace(text)
	if text == "" {
		return f, nil
	}
	repo, err := e.startRepo("")
	if err != nil {
		f.Err = err.Error()
	}
	if f.Err != "" {
		return f, nil
	}
	f.Repo = repo.Repo
	in := ParseStart(text, e.trackers(repo.Repo))
	switch {
	case in.PR != nil:
		f.Err = "a PR link: adoption comes in milestone 2"
		return f, nil
	case in.Run != nil:
		f.Err = "an Actions run link: CI tasks come in milestone 3"
		return f, nil
	}
	var tk *jira.Ticket
	words := in.Prompt
	switch {
	case in.Key != "":
		words = in.Guidance
		if t, err := e.reg.TaskByKey(ctx, in.Key); err == nil {
			f.Err = keyRuns{t}.Error()
			return f, nil
		}
		got, err := e.readTicket(ctx, in.Key)
		down := err != nil && jiraDown(err)
		switch {
		case down:
			f.Err = "Jira does not answer: the task waits for it, then starts"
		case err != nil:
			f.Err = err.Error()
		default:
			tk = &got
			f.Jira = e.jiraPreview(got)
		}
		if f.Err != "" && !down { // a refusal: no task would start
			return f, nil
		}
		f.Guidance = in.Guidance // what the start sends every agent, as ParseStart read it
	case len(in.Found) > 0 && e.d.Jira != nil:
		var stop bool
		if tk, stop = e.previewFound(ctx, &f, in); stop {
			return f, nil
		}
	}
	if tk == nil && in.Key == "" {
		f.Title = promptTitle(in.Prompt)
	}
	f.Kind, f.KindWhy = PickKind(e.cfg.Factory, tk, words)
	if login, err := e.operatorLogin(repo.Repo); err == nil {
		key, summary := in.Key, in.Prompt
		if tk != nil {
			key, summary = tk.Key, tk.Summary
		}
		if key == "" || tk != nil {
			f.Branch, _ = e.branchFor(ctx, 0, repo.Repo, login, key, summary)
		}
	}
	f.Starts = e.startsWhen(ctx)
	f.Agents = e.firstAgents(f.Kind)
	return f, nil
}

// previewFound previews a prompt whose words name keys elsewhere than at
// their start (in.Found), as startFound would start it: the ticket of the one
// key Jira knows (returned, for the kind and the branch of a key task), or,
// for several, their keys (f.Keys) and no ticket: the prompt task they are,
// which asks which issue it is. A key a task holds is f.Err and stops the
// preview; Jira's silence is f.Err and the preview goes on, as a key at the
// start does. A key Jira refuses for a cause of its own is never an error:
// the preview names the cause, "Jira refused PR-12345: ...; starts from your
// words", as f.Err on the JIRA line, in the note of the one key Jira knows
// beside it, or as f.Err under the several keys.
func (e *Engine) previewFound(ctx context.Context, f *tui.NewTaskForm, in StartInput) (tk *jira.Ticket, stop bool) {
	if t, err := e.holderOf(ctx, in.Found); err == nil {
		f.Err = keyRuns{t}.Error()
		return nil, true
	}
	known, refused, err := e.readFound(ctx, in.Found)
	switch {
	case err != nil && ctx.Err() != nil: // the request was cancelled
		f.Err = err.Error()
		return nil, true
	case err != nil: // silence, or the end of the time the reads share
		f.Err = "Jira does not answer: the task waits for it, then starts"
		return nil, false
	case len(known) == 0:
		if len(refused) > 0 {
			f.Err = refused[0].note(refusedAlone)
		}
		return nil, false
	}
	if len(known) > 1 {
		// The prompt task they are, which asks which issue it is: no ticket
		// is the preview's, the screen names the keys (f.Keys).
		for _, k := range known {
			f.Keys = append(f.Keys, k.Key)
		}
		if len(refused) > 0 {
			f.Err = refused[0].note(refusedBeside)
		}
		return nil, false
	}
	f.Jira = e.jiraPreview(known[0])
	f.Jira.Found = foundNote
	f.Guidance = in.Prompt // the whole text, as startFound sends it
	if len(refused) > 0 {
		f.Jira.Found += "; " + refused[0].note(refusedBeside)
	}
	return &known[0], false
}

// jiraPreview is the preview of the ticket the daemon read.
func (e *Engine) jiraPreview(got jira.Ticket) *tui.JiraPreview {
	desc := strings.Join(strings.Fields(got.Description), " ")
	excerpt := textx.Clip(desc, maxExcerpt)
	return &tui.JiraPreview{Key: got.Key, Type: got.Type, Status: got.Status, Priority: got.Priority, Summary: got.Summary,
		Excerpt: excerpt, Read: e.now(), More: excerpt != desc}
}

// readTicket reads a key's ticket for the preview; a refusal is in a
// start's words (jiraCause).
func (e *Engine) readTicket(ctx context.Context, key string) (jira.Ticket, error) {
	if e.d.Jira == nil {
		return jira.Ticket{}, fmt.Errorf("[jira] is off, so the factory cannot read %s", key)
	}
	tk, err := e.d.Jira.Ticket(ctx, key)
	if err != nil && !jiraDown(err) {
		return tk, jiraCause(key, err)
	}
	return tk, err
}

// startsWhen says when a task started now would start, as the new-task
// screen's STARTS line does.
func (e *Engine) startsWhen(ctx context.Context) string {
	tasks, err := e.reg.Tasks(ctx)
	if err != nil {
		return "unknown: " + err.Error()
	}
	mem := e.readMemory(ctx)
	switch who, _ := e.startGate(mem, tasks, 0); {
	case who == waitMemory && mem.critical:
		return "queued: memory full (critical pressure) · starts when it eases"
	case who == waitMemory:
		return fmt.Sprintf("queued: memory full (swap %d%%) · starts when swap is under %d%%", mem.pct, e.cfg.Factory.MemoryGateSwap)
	case who != "":
		return fmt.Sprintf("queued: all %d copies busy · starts when a task frees one", max(1, e.cfg.Factory.MaxActive))
	}
	if e.d.Copies != nil {
		if n, err := e.d.Copies.Free(ctx); err == nil {
			return "now, " + textx.Count(n, "copy", "copies") + " free"
		}
	}
	return "now"
}

// firstAgents are the whys of the roles of the kind's first agents stage,
// as the new-task screen's AGENTS line lists them.
func (e *Engine) firstAgents(kind string) []string {
	for _, st := range e.cfg.Factory.Kinds[kind].Stages {
		if st.Type != config.FactoryStageAgents {
			continue
		}
		var out []string
		for _, name := range st.Roles {
			if r, ok := e.cfg.FactoryRole(name); ok && r.Why != "" {
				out = append(out, r.Why)
			}
		}
		return out
	}
	return nil
}

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
// read for a key (or the reason it cannot: Err), the kind and why, the
// title of a prompt, the branch, when the task would start, and its first
// agents. It makes nothing.
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
	if in.Key != "" {
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
			desc := strings.Join(strings.Fields(got.Description), " ")
			excerpt := textx.Clip(desc, maxExcerpt)
			f.Jira = &tui.JiraPreview{Key: got.Key, Type: got.Type, Status: got.Status, Priority: got.Priority, Summary: got.Summary,
				Excerpt: excerpt, Read: e.now(), More: excerpt != desc}
		}
		if f.Err != "" && !down { // a refusal: no task would start
			return f, nil
		}
	} else {
		f.Title = promptTitle(in.Prompt)
	}
	f.Kind, f.KindWhy = PickKind(e.cfg.Factory, tk, words)
	if login, err := e.operatorLogin(repo.Repo); err == nil {
		key, summary := in.Key, in.Prompt
		if tk != nil {
			key, summary = tk.Key, tk.Summary
		}
		if key == "" || tk != nil {
			f.Branch, _ = e.branchFor(ctx, repo.Repo, login, key, summary)
		}
	}
	f.Starts = e.startsWhen(ctx)
	f.Agents = e.firstAgents(f.Kind)
	return f, nil
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

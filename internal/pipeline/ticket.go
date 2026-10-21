package pipeline

// The PR's Jira ticket (DECISIONS "A round hands the judge the PR's Jira
// ticket"): a first review checked the description's claims but never what
// the task asked for. With [jira] on, a round reads the issue the PR names
// once, before the stages of its first head, and writes it as ticket.md in
// each head's report directory; every judge prompt names the file
// (`ticket`), and the skill compares the description's scope and the PR's
// behaviour with it. The key is the first one a tracker on the [jira] site
// knows in the PR's title, else in its head branch (the description is not
// read: the round has no copy of it and reads none from GitHub). The issue
// is data, never in a prompt or an event: those carry the key, the status
// name, the number of requirement fields and sizes. A blind replay reads
// the ticket too: the requirements predate the PR, and no comment is read,
// so nothing written after the review reaches its judge.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// TicketFile is the PR's Jira issue (jira.Ticket.Markdown), in the round's
// report directory.
const TicketFile = "ticket.md"

// Tickets reads Jira issues: *jira.Client, made once per daemon start.
type Tickets interface {
	Ticket(ctx context.Context, key string) (jira.Ticket, error)
	// WarnTokenFile reports whether a token file problem is a warning:
	// true once per daemon start, then false (info), as the config's
	// warning already names it.
	WarnTokenFile() bool
}

// ticketKey is the Jira issue key pr names: the first key of one of the
// repository's trackers (Config.TrackersFor) whose template is on the
// [jira] site (Jira.Names; a Linear tracker never counts) in its title,
// else in its head branch; "" when neither names one.
func ticketKey(cfg *config.Config, repo string, pr store.PR) string {
	var trackers []config.Tracker
	for _, t := range cfg.TrackersFor(repo) {
		if cfg.Jira.Names(t.URL) {
			trackers = append(trackers, t)
		}
	}
	if len(trackers) == 0 {
		return ""
	}
	for _, text := range []string{store.Deref(pr.Title), store.Deref(pr.HeadRef)} {
		if key, _ := config.Issue(text, trackers); key != "" {
			return key
		}
	}
	return ""
}

// readTicket reads the issue the PR names (ticketKey), nil when there is
// none: [jira] off, no key (an info line), or a failure, which warns with
// its class (jira.Error) and goes on; a token file problem warns once per
// daemon start (Tickets.WarnTokenFile), then is info.
func (rd *round) readTicket(ctx context.Context) *jira.Ticket {
	if rd.r.Tickets == nil || !rd.r.Config.Jira.On() {
		return nil
	}
	key := ticketKey(rd.r.Config, rd.owner+"/"+rd.name, rd.in.PR)
	if key == "" {
		rd.event(ctx, "info", "round.ticket", fmt.Sprintf("no Jira issue key of a tracker on %s in the title or the head branch: no %s",
			rd.r.Config.Jira.URL, TicketFile), nil)
		return nil
	}
	t, err := rd.r.Tickets.Ticket(ctx, key)
	if err == nil {
		return &t
	}
	if ctx.Err() != nil {
		return nil
	}
	level, data := "warn", map[string]any{"key": key}
	if je, ok := errors.AsType[*jira.Error](err); ok {
		data["class"] = je.Class
		if je.Code != 0 {
			data["code"] = je.Code
		}
		if je.Class == jira.ClassTokenFile && !rd.r.Tickets.WarnTokenFile() {
			level = "info"
		}
	}
	rd.event(ctx, level, "round.ticket", fmt.Sprintf("could not read the Jira issue %s; the judge goes without it: %v", key, err), data)
	return nil
}

// writeTicket writes ticket.md for the head under review, beside its
// docs.json (a restart writes the new head's), reading the issue once a
// round (readTicket), and keeps its path for the judge prompts in
// rd.ticketFile; "" when there is none, and an earlier round's file on this
// head is removed. A continued turn reads nothing: its prompt names the
// paused turn's file when there is one. Each head's file is a round.ticket
// event naming the key, the status, the requirement fields and the sizes.
func (rd *round) writeTicket(ctx context.Context) {
	rd.ticketFile = ""
	path := filepath.Join(rd.dir, TicketFile)
	if rd.in.Kind == KindContinue {
		if fsx.Exists(path) {
			rd.ticketFile = path
		}
		return
	}
	if !rd.ticketRead {
		rd.ticketRead = true
		rd.ticket = rd.readTicket(ctx)
	}
	t := rd.ticket
	if t == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) && ctx.Err() == nil {
			rd.event(ctx, "warn", "round.ticket", fmt.Sprintf("could not remove an earlier round's %s: %v", TicketFile, err), nil)
		}
		return
	}
	md := t.Markdown()
	if err := fsx.WriteFileAtomic(path, md, 0o600); err != nil {
		if ctx.Err() == nil {
			rd.event(ctx, "warn", "round.ticket", fmt.Sprintf("could not write %s; the judge goes without it: %v", TicketFile, err), map[string]any{"key": t.Key})
		}
		return
	}
	rd.ticketFile = path
	status := textx.Clip(t.Status, 60)
	rd.event(ctx, "info", "round.ticket", fmt.Sprintf("%s: %s (%s), %s, %d bytes", TicketFile, t.Key, status,
		textx.Count(len(t.Fields), "requirement field", "requirement fields"), len(md)),
		map[string]any{"key": t.Key, "status": status, "fields": len(t.Fields), "description_bytes": t.DescriptionBytes, "bytes": len(md), "file": path})
}

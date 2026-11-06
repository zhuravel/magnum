package engine

// The move of magnum's panes to another herdr session. Each herdr server
// numbers its workspaces, tabs and panes from w1, so the registry's herdr
// ids are good only on the server that gave them: store.KVHerdrPanes names
// it, and the App opens its panes there (Deps.PaneSession), never where the
// config says. When [herdr] review_session names another session
// (config.Config.ReviewHerdr), the daemon moves them. Until then it holds
// whatever would open panes in the old session, as a drain does
// (panesMoving: rounds, retros, curations, the gate, fixes, whiteboards,
// opens). At the first tick with nothing in flight (restartForBuild) it
// rebinds the sessions of every PR that records a herdr id to the panes that
// hold their conversations (Agents.Recover), parks them and closes their
// retired panes, in the old session; forgets every id (store.ForgetHerdrIDs: the live rows
// become lost and resume by their conversations); records the new session;
// toasts once in your session; and exits (ErrRestartForPanes) for launchd to
// start the daemon there. A park that is refused, an old server that does
// not answer, or one that is not running (herdr.NoServer) while herdr ids
// are recorded holds the move to the next idle tick, recorded for `magnum
// status` (KVHerdrPanesMove) and toasted once it has waited
// panesMoveToastAfter: herdr restores a session's workspaces when its
// server starts again, and resumes their agents in their conversations, so
// the move waits for that server to park them there (a named old session
// the daemon opens itself, never yours). The record of the old session
// changes last, so a move cut short starts again at the next start, where
// the steps done already change nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// KVHerdrPanesMove is the move of magnum's panes that waits (PanesMove
	// as JSON): written by each idle tick whose try stopped short, read by
	// `magnum status` and doctor, deleted once the move is done or no longer
	// asked for.
	KVHerdrPanesMove = "herdr.panes_move"
	// panesMoveToastAfter is how long a move waits on a park or the old
	// server before a toast in your session says so, and panesMoveToastSpan
	// how often it says it again.
	panesMoveToastAfter = 30 * time.Minute
	panesMoveToastSpan  = 6 * time.Hour
)

// ErrRestartForPanes is what Tick and Run return once the daemon moved
// magnum's panes to the herdr session the config names: the exit is
// non-zero, so launchd starts it again, and the new daemon's App opens its
// panes there.
var ErrRestartForPanes = errors.New("magnum's panes moved to another herdr session and nothing is in flight; exiting for launchd to start the daemon there")

// PanesMove is a move of magnum's panes that waits (KVHerdrPanesMove): the
// session the registry's ids belong to and the one the config names, since
// when it waits, and why the last try stopped short: Subject is the PR
// whose park was refused ("" when the old server did not answer or ran
// none, the registry could not be read, or the restart was refused).
type PanesMove struct {
	From    herdr.Session `json:"from"`
	To      herdr.Session `json:"to"`
	Since   time.Time     `json:"since"`
	Subject string        `json:"subject,omitempty"`
	Reason  string        `json:"reason"`
}

// ReadPanesMove reads KVHerdrPanesMove (false when no move waits, or the
// record cannot be read).
func ReadPanesMove(ctx context.Context, st *store.Store) (PanesMove, bool) {
	v, ok, err := st.GetKV(ctx, KVHerdrPanesMove)
	if err != nil || !ok || v == "" {
		return PanesMove{}, false
	}
	var m PanesMove
	if json.Unmarshal([]byte(v), &m) != nil || m.Since.IsZero() {
		return PanesMove{}, false
	}
	return m, true
}

// sameServer reports whether two herdr sessions are one server: their
// sockets are one path.
func sameServer(a, b herdr.Session) bool {
	return filepath.Clean(a.Socket) == filepath.Clean(b.Socket)
}

// panesMoveDue is the move this daemon owes: from the session the
// registry's herdr ids belong to (Deps.PaneSession) to the one the config
// names, when they are two servers. None for a daemon that knows no
// session (a replay's engine), a dry run, `magnum daemon --once` (no
// launchd starts it again) or one with no agents to park.
func (e *Engine) panesMoveDue() (from, to herdr.Session, ok bool) {
	from = e.d.PaneSession
	if e.d.DryRun || e.once || e.d.Agents == nil || e.d.Herdr == nil || from.Socket == "" || e.cfg == nil {
		return herdr.Session{}, herdr.Session{}, false
	}
	to = e.cfg.ReviewHerdr()
	return from, to, !sameServer(from, to)
}

// panesMoving is why the daemon holds new work for a move of magnum's
// panes ("" = none is due): stopping and draining count it.
func (e *Engine) panesMoving() string {
	from, to, ok := e.panesMoveDue()
	if !ok {
		return ""
	}
	return fmt.Sprintf("moving magnum's panes from herdr session %s to %s", from.Name, to.Name)
}

// recordPanes writes, at startup, the herdr session the registry's ids
// belong to when nothing records it yet: the App's (Deps.PaneSession), your
// session while the registry recorded none, where every build before the
// record opened its panes. A record stays, but for its name while the
// config names the same server (a renamed [terminal] session: the reveals
// use the name), and a move's wait is dropped once the config no longer
// asks for one. A dry run and an engine that knows no session (a replay's)
// write nothing.
func (e *Engine) recordPanes(ctx context.Context) {
	s := e.d.PaneSession
	if e.d.DryRun || s.Socket == "" || e.cfg == nil {
		return
	}
	if want := e.cfg.ReviewHerdr(); sameServer(s, want) {
		e.delKV(ctx, KVHerdrPanesMove)
		s.Name = want.Name
	}
	if v, ok := e.getKV(ctx, store.KVHerdrPanes); ok {
		var rec herdr.Session
		if json.Unmarshal([]byte(v), &rec) == nil && rec == s {
			return
		}
		if !sameServer(rec, s) {
			return // the App refuses a record it cannot read; one of another server is the move's to change
		}
	}
	if raw, err := json.Marshal(s); err == nil {
		e.setKV(ctx, store.KVHerdrPanes, string(raw))
	}
}

// movePanes moves magnum's panes from session from to session to, at an
// idle tick (restartForBuild): every PR whose sessions record a herdr id is
// parked and its retired panes closed in the old session, under the PR's
// reservation; then every id is forgotten and the new session recorded. An
// old server that is not running (herdr.NoServer), before or during the
// parks, holds the move as one that does not answer does (moveNoServer):
// herdr restores the panes when it starts. It reports whether the
// move is done; when not, the try's wait is recorded (panesMoveWaits) and
// the next idle tick tries again. A PR whose round is in flight is never
// parked: restartBusy keeps the move off while any is, and the PR's own
// state and reservation are checked again.
func (e *Engine) movePanes(ctx context.Context, from, to herdr.Session) bool {
	if _, waiting := ReadPanesMove(ctx, e.st); !waiting && !e.movingBegun {
		e.event(ctx, "info", "", "herdr.panes_move_begin",
			fmt.Sprintf("moving magnum's panes from herdr session %s to %s: parking every PR's sessions in %s", from.Name, to.Name, from.Name),
			map[string]any{"from": from.Name, "to": to.Name})
	}
	e.movingBegun = true
	prs, err := e.st.PRsWithHerdrIDs(ctx)
	if err != nil {
		return e.panesMoveWaits(ctx, from, to, "", "the registry cannot be read: "+err.Error())
	}
	if len(prs) > 0 {
		if _, err := e.d.Herdr.Snapshot(ctx); err != nil {
			if herdr.NoServer(err) {
				return e.moveNoServer(ctx, from, to, err)
			}
			return e.panesMoveWaits(ctx, from, to, "", fmt.Sprintf("herdr session %s does not answer: %v", from.Name, err))
		}
	}
	var held struct{ subject, reason string }
	for _, id := range prs {
		subject, reason, err := e.parkForMove(ctx, id, to)
		switch {
		case ctx.Err() != nil:
			return false // the daemon stops: the next start moves them
		case herdr.NoServer(err):
			return e.moveNoServer(ctx, from, to, err) // the old server went away meanwhile
		case reason != "" && held.reason == "":
			held.subject, held.reason = subject, reason
		}
	}
	if held.reason != "" {
		return e.panesMoveWaits(ctx, from, to, held.subject, held.reason)
	}
	n, err := e.st.ForgetHerdrIDs(ctx)
	if err != nil {
		return e.panesMoveWaits(ctx, from, to, "", "the registry cannot forget the herdr ids: "+err.Error())
	}
	raw, _ := json.Marshal(to)
	if err := e.st.SetKV(ctx, store.KVHerdrPanes, string(raw)); err != nil {
		return e.panesMoveWaits(ctx, from, to, "", "the registry cannot record the new session: "+err.Error())
	}
	e.delKV(ctx, KVHerdrPanesMove)
	how := textx.Count(len(prs), "PR's sessions", "PRs' sessions") + " parked in " + from.Name
	e.event(ctx, "info", "", "herdr.panes_moved",
		fmt.Sprintf("magnum's panes moved from herdr session %s to herdr session %s (%s; %d sessions forgot their herdr ids and resume by their conversations); the daemon restarts there",
			from.Name, to.Name, how, n),
		map[string]any{"from": from.Name, "to": to.Name, "prs": len(prs), "sessions": n})
	e.urgent("panes-moved:"+to.Name, "magnum: its review panes moved to herdr session "+to.Name,
		"New rounds open there; `magnum open` reveals it. The conversations resume when the PRs' rounds or opens need them.",
		time.Hour)
	return true
}

// parkForMove parks PR id's sessions and closes its retired panes in the
// old session, under the PR's reservation, and returns the PR's audit
// subject and why it could not ("" = parked): its round in flight, a
// reservation another job holds, or a recovery or park refused or failed,
// whose error it returns too. A PR the registry no longer has is nothing to
// park. Its sessions are first rebound to the panes that hold their
// conversations (Agents.Recover): a server that started again since the
// daemon's start restored its workspaces under new ids and resumed their
// agents, some without the names magnum gave them, and Park finds a
// session's agent only by its name, its pane or its workspace, so such an
// agent would go on in the old session.
func (e *Engine) parkForMove(ctx context.Context, id int64, to herdr.Session) (subject, reason string, err error) {
	pr, err := e.st.PRByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", nil
	}
	if err != nil {
		return fmt.Sprintf("pr:%d", id), "the registry cannot read it: " + err.Error(), err
	}
	subject = e.prLabelSubject(ctx, pr)
	if slices.Contains(store.InFlightStates, pr.State) {
		return subject, "its round is " + pr.State, nil
	}
	if !e.reserveSlotWork(pr.ID, "its sessions are being parked to move magnum's panes to herdr session "+to.Name) {
		return subject, e.heldReason(pr.ID), nil
	}
	defer e.endEviction(pr.ID)
	_, err = e.d.Agents.Recover(ctx, pr)
	if err == nil {
		err = e.d.Agents.Park(ctx, pr)
	}
	if err == nil {
		err = e.d.Agents.CloseRetiredPanes(ctx, pr)
	}
	if err != nil {
		return subject, err.Error(), err
	}
	return subject, "", nil
}

// moveNoServer holds the move while no server runs in the old session
// (herdr.NoServer) and herdr ids are recorded: herdr restores a session's
// workspaces when its server starts again and resumes their agents in
// their conversations, so ids forgotten now would leave those agents to
// nobody, and a round resuming one of the conversations in the new session
// would run it twice. A named old session the daemon opens to start its
// server (openPaneSession, at most every herdrOpenGap); yours it never
// opens (FromApp sets no opener while the panes are there), and the wait
// names the command that starts it. Once the server answers, the next idle
// tick parks what it restored.
func (e *Engine) moveNoServer(ctx context.Context, from, to herdr.Session, cause error) bool {
	how := "start it (`" + herdrCommand(from.Name) + "`)"
	if e.d.OpenHerdr != nil {
		how = "the daemon opens `" + herdrCommand(from.Name) + "` to start it"
		e.openPaneSession(ctx, cause, "the move of magnum's panes to herdr session "+to.Name+" waits to park what it restores")
	}
	return e.panesMoveWaits(ctx, from, to, "", fmt.Sprintf(
		"no herdr server runs in session %s, which restores magnum's panes when it starts: %s, and the move parks them there",
		from.Name, how))
}

// herdrCommand is the command that opens a client of herdr session name,
// which starts its server: `herdr` for the default session.
func herdrCommand(name string) string {
	if name == "" || name == "default" {
		return "herdr"
	}
	return "herdr --session " + name
}

// panesMoveWaits records why the move's try stopped short (KVHerdrPanesMove,
// which `magnum status` reads), says so in an event once per reason, and
// toasts in your session once the move has waited panesMoveToastAfter. It
// reports false: the move is not done.
func (e *Engine) panesMoveWaits(ctx context.Context, from, to herdr.Session, subject, reason string) bool {
	if ctx.Err() != nil {
		return false
	}
	reason = execx.Redact(reason)
	now := e.now()
	mv := PanesMove{From: from, To: to, Since: now, Subject: subject, Reason: reason}
	prev, had := ReadPanesMove(ctx, e.st)
	if had && prev.From == from && prev.To == to {
		mv.Since = prev.Since
	}
	on := "" // the PR it waits on; the reason says what else it waits on
	if subject != "" {
		on = " on " + strings.TrimPrefix(subject, "pr:")
	}
	if !had || prev.Subject != subject || prev.Reason != reason {
		e.event(ctx, "warn", subject, "herdr.panes_move_waits",
			fmt.Sprintf("the move of magnum's panes from herdr session %s to %s waits%s: %s; new rounds wait too, and the next tick with nothing in flight tries again",
				from.Name, to.Name, on, reason),
			map[string]any{"from": from.Name, "to": to.Name})
	}
	if raw, err := json.Marshal(mv); err == nil {
		e.setKV(ctx, KVHerdrPanesMove, string(raw))
	}
	if now.Sub(mv.Since) >= panesMoveToastAfter {
		e.urgent("panes-move-waits:"+to.Name, "magnum: its panes wait to move to herdr session "+to.Name,
			fmt.Sprintf("Since %s%s: %s. No round starts until they move.", mv.Since.Local().Format("15:04"), on, reason),
			panesMoveToastSpan)
	}
	return false
}

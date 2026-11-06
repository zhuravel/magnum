package runtime

import (
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// launchLiaison starts the rig's task's liaison on its own owner row, in a
// workspace of its own in the task's folder, as Talk does, and returns the
// row.
func (g *rig) launchLiaison() store.PR {
	g.t.Helper()
	pr, err := g.r.liaisonRow(g.ctx, g.task)
	if err != nil {
		g.t.Fatalf("liaisonRow: %v", err)
	}
	dir := g.layout.FactoryTaskDir(g.task.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		g.t.Fatal(err)
	}
	role := g.role("fx-liaison")
	ws, err := g.r.mgr.EnsureWorkspace(g.ctx, pr, dir, nil, Tag+" liaison "+taskKey(g.task), []config.Role{role})
	if err != nil {
		g.t.Fatalf("EnsureWorkspace: %v", err)
	}
	if err := g.r.mgr.StartAgent(g.ctx, pr, role, ws.Panes[agents.Role(role.Name)], ""); err != nil {
		g.t.Fatalf("StartAgent: %v", err)
	}
	return pr
}

// liaisonAgent is the herdr name of the rig's task's liaison.
func (g *rig) liaisonAgent() string {
	return agents.TaggedAgentName(Tag, "factory/liaison", int(g.task.ID), agents.Role("fx-liaison"))
}

// The liaison's owner row is apart from its task's: repository
// factory/liaison, node id factory:liaison:<id>, the task's id as its
// number, its URL the task's folder. It is created once, OwnerTask names no
// task by it (the copies' reservations), and the task's sessions (the
// screen's, the CLI's) are not the liaison's.
func TestTheLiaisonsRowIsApartFromTheTasks(t *testing.T) {
	g := newRig(t)
	g.launch("fx-writer")
	pr := g.launchLiaison()

	id := strconv.FormatInt(g.task.ID, 10)
	if pr.NodeID != "factory:liaison:"+id || pr.Number != int(g.task.ID) || pr.HeadSHA != "t"+id ||
		pr.URL != "file://"+g.layout.FactoryTaskDir(g.task.ID) {
		t.Fatalf("the liaison's row = %s #%d head %s at %s", pr.NodeID, pr.Number, pr.HeadSHA, pr.URL)
	}
	repo, err := g.st.RepoByID(g.ctx, pr.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	if repo.NodeID != "factory:liaison" || repo.Owner != "factory" || repo.Name != "liaison" {
		t.Fatalf("the liaison's repository = %s %s/%s", repo.NodeID, repo.Owner, repo.Name)
	}
	if again, err := g.r.liaisonRow(g.ctx, g.task); err != nil || again.ID != pr.ID {
		t.Fatalf("liaisonRow again = row %d, %v; want row %d", again.ID, err, pr.ID)
	}
	if pr.ID == g.owner().ID {
		t.Fatal("the liaison shares the task's owner row")
	}
	if got, ok := LiaisonTask(pr.NodeID); !ok || got != g.task.ID {
		t.Fatalf("LiaisonTask(%s) = %d, %v", pr.NodeID, got, ok)
	}
	if _, ok := OwnerTask(pr.NodeID); ok {
		t.Fatalf("OwnerTask(%s) names a task", pr.NodeID)
	}
	if _, ok := LiaisonTask(g.owner().NodeID); ok {
		t.Fatalf("LiaisonTask(%s) names a task", g.owner().NodeID)
	}
	ss, err := TaskSessions(g.ctx, g.st, g.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Role != "fx-writer" {
		t.Fatalf("the task's sessions = %+v, want fx-writer's alone", ss)
	}
}

// Chat with the liaison is yours on the liaison's row alone: herdr showing
// you at work in its pane sets that row's human_active_at, never the
// task's, so the writer is not held (Send is not refused) and nobody is
// "you" at work in the task's copy.
func TestChatWithTheLiaisonLeavesTheTasksRowAlone(t *testing.T) {
	g := newRig(t)
	g.launch("fx-writer")
	liaison := g.launchLiaison()
	g.clock.Add(2 * time.Minute) // past a start's grace: herdr showing the agent at work is you

	g.h.focus(g.liaisonAgent())
	g.h.setStatus(g.liaisonAgent(), herdr.StatusWorking) // you type, no prompt of the factory's
	g.observe()
	g.h.setStatus(g.liaisonAgent(), herdr.StatusIdle)

	pr, err := g.st.PRByID(g.ctx, liaison.ID)
	if err != nil {
		t.Fatal(err)
	}
	if at := pr.HumanActiveAt; at == nil || !at.Equal(g.clock.Now()) {
		t.Fatalf("the liaison's row human_active_at = %v, want %v", at, g.clock.Now())
	}
	if at := g.owner().HumanActiveAt; at != nil {
		t.Fatalf("the task's row human_active_at = %v after your chat with the liaison, want none", at)
	}
	if role, you, err := g.r.AtWork(g.ctx, g.task); err != nil || role != "" || you {
		t.Fatalf("AtWork while you chat with the liaison = %q, %v, %v; want nobody", role, you, err)
	}
	g.send("fx-writer", "write the change") // fails the test on a refusal
}

// The liaison's session is no role of the task's attempt: a dialog it shows
// (its own AskUserQuestion, which magnum leaves to you) is no EventBlocked
// of the task, and its turn that ends is no run of the task's that ended.
func TestALiaisonsEventsNeverReachTheTasksRoles(t *testing.T) {
	g := newRig(t)
	g.launch("fx-writer")
	pr := g.launchLiaison()
	name := g.liaisonAgent()

	g.h.screen(name, "Which fix do you want?\n❯ 1. Retry the job (Recommended)\n  2. Skip it\nEnter to select · Esc to cancel\n")
	g.h.setStatus(name, herdr.StatusBlocked)
	if evs := g.observeN(3); len(evs) != 0 {
		t.Errorf("the liaison's dialog gave %+v, want nothing for the task", evs)
	}

	g.h.screen(name, "")
	g.h.setStatus(name, herdr.StatusIdle)
	g.observe()
	id, err := g.r.mgr.Prompt(g.ctx, pr, agents.Role("fx-liaison"), store.RunInitial, "talk with the operator about PR-12345")
	if err != nil {
		t.Fatalf("Prompt the liaison: %v", err)
	}
	g.observe()
	g.h.setStatus(name, herdr.StatusIdle)
	if evs := g.observeN(2); len(evs) != 0 {
		t.Fatalf("the liaison's ended turn gave %+v, want nothing for the task", evs)
	}
	if run := g.storeRun(id); run.State != store.RunEnded {
		t.Fatalf("the liaison's run is %s, want ended", run.State)
	}
	rrs, err := g.reg.RoleRuns(g.ctx, g.attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(rrs, func(rr registry.RoleRun) bool { return rr.Role == "fx-liaison" }) {
		t.Fatalf("the task's attempt records the liaison's run: %+v", rrs)
	}
}

package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// fieldIndex is the index of the fixture's setting t.
func fieldIndex(t *testing.T, d *SettingsData, target string) int {
	t.Helper()
	for i, f := range d.Fields {
		if f.Target == target {
			return i
		}
	}
	t.Fatalf("no setting %s", target)
	return -1
}

// stepOnly feeds msg to m without running the command it returns, which is
// handed back.
func stepOnly(t *testing.T, m settingsModel, msg tea.Msg) (settingsModel, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(settingsModel), cmd
}

// A cancelled spend picker leaves the grid on the draft's roles at once,
// and a role popup opened right after edits the draft's level, never the
// one the picker previewed.
func TestSettingsARolePopupAfterACancelledPreviewEditsTheDraftsLevel(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, selectRow(t, m, "spend level"), "enter", "j")
	mustContain(t, viewOf(m), "preview: burn")
	m, _ = stepOnly(t, m, keyMsg("esc")) // its command (if any) never runs
	mustContain(t, viewOf(m), "ROLES AFTER RESTART · spend high")
	m = press(t, selectRow(t, m, "claude-simplify"), "enter")
	if m.popup == nil || m.popup.level != "high" {
		t.Fatalf("the role popup edits level %q, want the draft's high", m.popup.level)
	}
}

// A list's entries stay as the file has them: an untouched entry with
// doubled spaces is staged back byte for byte, and only shown cleaned.
func TestSettingsKeepsAListsEntriesAsTheFileHasThem(t *testing.T) {
	src := newFakeSettings()
	i := fieldIndex(t, src.data, "repo.talkable/web.required_checks")
	src.data.Fields[i].List = []string{"ci  /  rspec", "ci / lint"}
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{Scope: "repo:talkable/web"}, 120, 30)
	m = press(t, selectRow(t, m, "required checks"), "enter", "a")
	m = typeIn(t, m, "deploy", "enter")
	got := m.draft["repo.talkable/web.required_checks"].List
	if !slices.Equal(got, []string{"ci  /  rspec", "deploy", "ci / lint"}) {
		t.Fatalf("staged %q, want the untouched entries byte for byte", got)
	}
	mustContain(t, viewOf(m), "ci / rspec")
}

// An empty list shows what it stands for, never a bare "none" where it
// means every author.
func TestSettingsAnEmptyListShowsWhatItMeans(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, 120, 30)
	m = m.stage("watch.1.auto_approve_authors", SettingsEdit{List: []string{}})
	v := viewOf(m)
	mustContain(t, v, "auto-approve authors    every author")
	if strings.Contains(v, "auto-approve authors    none") {
		t.Errorf("an emptied allowlist reads none:\n%s", v)
	}
}

// Duplicates follow the setting's own matching: logins are one whatever
// their case, check names are not.
func TestSettingsListDuplicatesFollowTheSettingsMatching(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "repo:talkable/web"}, 120, 30)
	m = press(t, selectRow(t, m, "required checks"), "enter", "a")
	m = typeIn(t, m, "ci / LINT", "enter")
	if got := m.draft["repo.talkable/web.required_checks"].List; !slices.Contains(got, "ci / LINT") {
		t.Errorf("a check name differing in case is another check: %q", got)
	}
	m = testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, 120, 30)
	m = press(t, selectRow(t, m, "auto-approve authors"), "enter", "a")
	m = typeIn(t, m, "ALICE", "enter")
	mustContain(t, viewOf(m), "already in the list")
}

// huh's g and G move a Select's cursor without writing its value: the
// screen reads the hovered option, so the description, the preview and the
// staged value follow them.
func TestSettingsGAndGMoveTheChoiceItsDescriptionAndThePreview(t *testing.T) {
	src := newFakeSettings()
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, selectRow(t, m, "spend level"), "enter", "G")
	if m.popup.choice != "burn" {
		t.Fatalf("G hovers %q, want burn", m.popup.choice)
	}
	mustContain(t, viewOf(m), "burn: starts from medium", "preview: burn")
	if last := src.previews[len(src.previews)-1]; last["pipeline.spend"].Text != "burn" {
		t.Errorf("the preview resolves %+v", last)
	}
	m = press(t, m, "g")
	if m.popup.choice != optDefault {
		t.Errorf("g hovers %q, want the default", m.popup.choice)
	}
	m = press(t, m, "G", "enter")
	if m.draft["pipeline.spend"].Text != "burn" {
		t.Errorf("G then enter stages %+v", m.draft)
	}
}

// The hovered option's description is the screen's own work, done at once:
// no huh evaluation runs on another goroutine reading the bound value.
func TestSettingsDescribesTheHoveredOptionAtOnce(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, selectRow(t, m, "spend level"), "enter")
	m, _ = stepOnly(t, m, keyMsg("j")) // no command run
	mustContain(t, viewOf(m), "burn: starts from medium")
}

// A row that inherits ("inherit (high)") follows a staged edit of what it
// inherits.
func TestSettingsAnInheritedRowFollowsAStagedLevel(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	mustContain(t, viewOf(m), "codex spend        inherit (high)")
	m = m.stage("pipeline.spend", SettingsEdit{Text: "burn"})
	mustContain(t, viewOf(m), "codex spend        inherit (burn)")
}

// An answer to an older runtime read never overwrites a newer state.
func TestSettingsDropsAnOlderRuntimeAnswer(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 30)
	m = toSection(t, m, sectionRuntime)
	cmd := m.runtimeCmd() // a newer read is out
	old := &RuntimeState{Automation: RuntimeEntry{Name: "automation", Paused: true, State: "paused: stale"}}
	m = pump(t, m, settingsAsync{from: m.id, msg: settingsRuntimeMsg{state: old, gen: m.rtGen - 1}}).m
	if strings.Contains(viewOf(m), "paused: stale") {
		t.Fatal("an older runtime answer was taken")
	}
	m = pump(t, m, settingsCmd(t, cmd)...).m
	mustContain(t, viewOf(m), "running")
}

// A popup's commands are tagged with it: what a cancelled popup's form
// answers late never reaches, let alone completes, the next one.
func TestSettingsAClosedPopupsAnswersNeverReachTheNext(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 30)
	m = press(t, selectRow(t, m, "spend level"), "enter", "j")
	m, late := stepOnly(t, m, keyMsg("enter")) // the form moves on through a command not run yet
	if m.popup == nil {
		t.Fatal("the form completed without its command")
	}
	m, _ = stepOnly(t, m, keyMsg("esc"))
	m = press(t, selectRow(t, m, "codex spend"), "enter")
	first := m.popup
	m = pump(t, m, settingsCmd(t, late)...).m
	if m.popup != first {
		t.Fatalf("a late answer of the cancelled popup closed the next one (draft %+v)", m.draft)
	}
	if len(m.draft) != 0 {
		t.Errorf("a late answer staged %+v", m.draft)
	}
}

// An open popup follows a resize: its width comes again from the screen.
func TestSettingsAPopupFollowsAResize(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, m, "c") // the scope picker: its widest line is long
	wide := m.popup.width
	m = pump(t, m, tea.WindowSizeMsg{Width: 60, Height: 30}).m
	if m.popup.width >= wide || m.popup.width > m.listWidth()-4 {
		t.Errorf("the popup is %d wide after the resize (was %d, the list %d)", m.popup.width, wide, m.listWidth())
	}
	for i, l := range strings.Split(m.View().Content, "\n") {
		if ansi.StringWidth(l) > 60 {
			t.Errorf("line %d is %d wide on a 60-column screen", i+1, ansi.StringWidth(l))
		}
	}
}

// An open popup takes a new background's palette.
func TestSettingsAPopupTakesANewBackground(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 30)
	m = press(t, selectRow(t, m, "spend level"), "enter")
	dark := m.View().Content
	m = pump(t, m, tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")}).m
	light := m.View().Content
	if !strings.Contains(dark, "\x1b[94m› ") || !strings.Contains(light, "\x1b[34m› ") {
		t.Errorf("the popup's cursor keeps its palette:\ndark %q\nlight %q", grepLine(dark, "high (saved)"), grepLine(light, "high (saved)"))
	}
}

// grepLine is the first line of s holding want.
func grepLine(s, want string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if strings.Contains(ansi.Strip(l), want) {
			return l
		}
	}
	return ""
}

// An effort the picker does not list stays the choice it starts on, as
// saved, so stepping through the popup changes nothing.
func TestSettingsAnEffortOffThePickersListStaysAsSaved(t *testing.T) {
	src := newFakeSettings()
	i := fieldIndex(t, src.data, "spend.high.claude-review.effort")
	src.data.Fields[i].Text, src.data.Fields[i].Value = "max", "max"
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, selectRow(t, m, "claude-review"), "enter", "enter", "enter")
	mustContain(t, viewOf(m), "max (saved)")
	if m.popup.first != "max" {
		t.Fatalf("the first effort starts on %q, want max", m.popup.first)
	}
	m = press(t, m, "enter", "enter")
	if m.popup != nil || len(m.draft) != 0 {
		t.Errorf("stepping through stages %+v", m.draft)
	}
}

// A save refused because the file changed on disk offers to read it again
// keeping the staged edits; an edit of a watch whose position now holds
// another owner is dropped and said so.
func TestSettingsOffersToReadAChangedFileAgainKeepingTheDraft(t *testing.T) {
	src := newFakeSettings()
	src.planErr = fmt.Errorf("%w: settings: the config file changed", ErrSettingsChanged)
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = m.stage("watch.2.include_drafts", SettingsEdit{Text: "true"})
	m = press(t, m, "s")
	mustContain(t, viewOf(m), "The file changed on disk. Read it again and keep the 2 staged edits? y/N")
	src.mu.Lock()
	src.planErr = nil
	src.data.Scopes = slices.Clone(src.data.Scopes)
	src.data.Scopes[1].Name = "elsewhere"
	src.mu.Unlock()
	kept := press(t, m, "n")
	if len(kept.draft) != 2 || kept.loads() != 1 {
		t.Errorf("n keeps the draft and reads nothing: %+v", kept.draft)
	}
	m = press(t, m, "y")
	if _, ok := m.draft["usage.codex_soft"]; !ok || len(m.draft) != 1 {
		t.Fatalf("after the re-read the draft is %+v, want codex_soft alone", m.draft)
	}
	mustContain(t, viewOf(m), "kept 1 staged edit, dropped 1")
	m = press(t, m, "s")
	if m.mode != modeSave {
		t.Errorf("the kept draft saves again")
	}
}

// loads counts the screen's reads of the file (for the re-read test).
func (m settingsModel) loads() int {
	if f, ok := m.src.(*fakeSettings); ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.loadCount
	}
	return 0
}

// A paste reaches the open Input, and the search being typed.
func TestSettingsPastesIntoTheOpenInputAndTheSearch(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 30)
	m = toSection(t, m, "Reviews & agents")
	m = press(t, selectRow(t, m, "park idle after"), "enter")
	m = pump(t, m, tea.PasteMsg{Content: "45m"}).m
	m = press(t, m, "enter")
	if m.draft["daemon.park_idle_after"].Text != "45m" {
		t.Errorf("the pasted duration staged %+v", m.draft)
	}
	m = press(t, m, "/")
	m = pump(t, m, tea.PasteMsg{Content: "quiet"}).m
	if m.query != "quiet" {
		t.Errorf("the search reads %q after a paste", m.query)
	}
}

// A loader refusal marks the setting whose whole key path it names, not
// another one sharing its last word.
func TestSettingsMarksOnlyTheSettingTheLoaderNames(t *testing.T) {
	src := newFakeSettings()
	src.planErr = fmt.Errorf("daemon.own_min_rereview_interval: must not be below the re-review interval")
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("daemon.min_rereview_interval", SettingsEdit{Text: "10m"})
	m = m.stage("watch.1.own_min_rereview_interval", SettingsEdit{Text: "1m"})
	m = press(t, m, "s")
	if m.invalid["watch.1.own_min_rereview_interval"] != "" || m.invalid["daemon.min_rereview_interval"] != "" {
		t.Errorf("marked %v: neither names the refused key", m.invalid)
	}
}

// Emptying a list the file does not set stages nothing: there is nothing
// to write.
func TestSettingsEmptyingAListTheFileLeavesUnsetStagesNothing(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:2"}, 120, 30)
	m = press(t, selectRow(t, m, "skip authors"), "enter", "a")
	m = typeIn(t, m, "alice", "enter")
	if len(m.draft) != 1 {
		t.Fatalf("adding stages the list: %+v", m.draft)
	}
	m = press(t, m, "k", "x")
	if len(m.draft) != 0 {
		t.Errorf("an emptied unset list stages %+v", m.draft)
	}
}

// The Runtime section numbers the watches as the scope picker and the
// Watches section do: by the file's [[watch]] blocks.
func TestSettingsNumbersRuntimeWatchesAsItsScopes(t *testing.T) {
	rt := newFakeRuntime()
	rt.state.Watches[1].Label = "watch 7 example"
	m := testSettings(t, newFakeSettings(), rt, SettingsOptions{}, 120, 30)
	m = toSection(t, m, sectionRuntime)
	mustContain(t, viewOf(m), "watch 2 example")
	if strings.Contains(viewOf(m), "watch 7") {
		t.Errorf("the source's numbering shows:\n%s", viewOf(m))
	}
}

// A role popup takes its level from the draft's own roles: while the
// draft's roles are still on their way (a level just staged) it waits,
// and a candidate a popup previewed never decides it.
func TestSettingsARolePopupWaitsForTheDraftsRolesAndIgnoresACandidate(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, selectRow(t, m, "spend level"), "enter", "j")
	m, staged := stepOnly(t, m, keyMsg("enter"))
	var held []tea.Msg
	for queue := settingsCmd(t, staged); len(queue) > 0; {
		msg := queue[0]
		queue = queue[1:]
		if a, ok := msg.(settingsAsync); ok {
			if _, preview := a.msg.(settingsPreviewMsg); preview {
				held = append(held, msg) // the draft's roles, held back
				continue
			}
		}
		var cmd tea.Cmd
		m, cmd = stepOnly(t, m, msg)
		queue = append(queue, settingsCmd(t, cmd)...)
	}
	if m.draft["pipeline.spend"].Text != "burn" || len(held) == 0 {
		t.Fatalf("burn is not staged with its preview out: %+v (held %d)", m.draft, len(held))
	}
	m = press(t, selectRow(t, m, "claude-simplify"), "enter")
	if m.popup != nil {
		t.Fatalf("the role popup opened at level %q before the draft's roles came", m.popup.level)
	}
	mustContain(t, viewOf(m), "wait: resolving the roles of the draft")
	m = pump(t, m, held...).m
	m = press(t, m, "enter")
	if m.popup == nil || m.popup.level != "burn" {
		t.Fatalf("the role popup edits %+v, want the draft's burn", m.popup)
	}

	// A candidate in the grid (as an open popup shows one) does not decide
	// the level.
	m = testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	cand := slices.Clone(m.preview)
	for i := range cand {
		cand[i].Level = "burn"
	}
	m.preview, m.previewCand = cand, "preview: burn"
	m = press(t, selectRow(t, m, "claude-simplify"), "enter")
	if m.popup == nil || m.popup.level != "high" {
		t.Fatalf("the role popup follows a candidate: %+v", m.popup)
	}
}

// sameOwnerWatches makes the fixture's second watch another of talkable's,
// with its own repositories.
func sameOwnerWatches(src *fakeSettings) {
	src.data.Scopes = slices.Clone(src.data.Scopes)
	src.data.Scopes[1].Name, src.data.Scopes[1].Repos = "talkable", []string{"talkable/docs"}
}

// Two watches of one owner swapped by hand: a re-read drops the staged edit
// of the watch whose position the other now holds, though the owner at that
// position did not change.
func TestSettingsAReReadDropsAnEditOfAWatchWhoseTwinTookItsPlace(t *testing.T) {
	src := newFakeSettings()
	sameOwnerWatches(src)
	src.planErr = fmt.Errorf("%w: settings: the config file changed", ErrSettingsChanged)
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("watch.1.include_drafts", SettingsEdit{Text: "true"})
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = press(t, m, "s")
	src.mu.Lock()
	src.planErr = nil
	src.data.Scopes = slices.Clone(src.data.Scopes)
	src.data.Scopes[0].Repos, src.data.Scopes[1].Repos = src.data.Scopes[1].Repos, src.data.Scopes[0].Repos
	src.mu.Unlock()
	m = press(t, m, "y")
	if _, kept := m.draft["watch.1.include_drafts"]; kept || len(m.draft) != 1 {
		t.Errorf("the swapped watch's edit survived the re-read: %+v", m.draft)
	}
}

// Any reload that meets staged edits checks that each watch is still the
// one they were made on, not only the re-read the save offered.
func TestSettingsEveryReloadWithStagedEditsChecksTheWatches(t *testing.T) {
	src := newFakeSettings()
	sameOwnerWatches(src)
	m := testSettings(t, src, newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = m.stage("watch.1.include_drafts", SettingsEdit{Text: "true"})
	swapped := *m.data
	swapped.Scopes = slices.Clone(swapped.Scopes)
	swapped.Scopes[0].Repos, swapped.Scopes[1].Repos = swapped.Scopes[1].Repos, swapped.Scopes[0].Repos
	m = pump(t, m, settingsAsync{from: m.id, msg: settingsLoadedMsg{data: &swapped}}).m
	if len(m.draft) != 0 {
		t.Errorf("a reload kept an edit on a watch now elsewhere: %+v", m.draft)
	}
}

// After a save the keys that change the draft wait until the saved file's
// rows are read: nothing is staged against the rows it replaces.
func TestSettingsWaitsForTheSavedFileBeforeTheNextEdit(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 120, 34)
	m = toSection(t, m, "Usage caps")
	m = m.stage("usage.codex_soft", SettingsEdit{Text: "80"})
	m = press(t, m, "s", "s")
	m, commit := stepOnly(t, m, keyMsg("y"))
	var reload []tea.Msg
	for _, msg := range settingsCmd(t, commit) {
		var cmd tea.Cmd
		m, cmd = stepOnly(t, m, msg)
		reload = append(reload, settingsCmd(t, cmd)...) // the saved file's read, held back
	}
	if len(reload) == 0 {
		t.Fatal("the save read nothing back")
	}
	m = press(t, m, "n") // the restart question
	m = press(t, selectRow(t, m, "codex soft cap"), "enter")
	if m.popup != nil {
		t.Fatal("a popup opened before the saved file was read")
	}
	mustContain(t, viewOf(m), "reading the file")
	m = pump(t, m, reload...).m
	m = press(t, m, "enter")
	if m.popup == nil {
		t.Error("the popup waits for nothing once the file is read")
	}
}

// Each item of a list has its own form: an earlier item's late answer
// never submits the next one.
func TestSettingsALateAnswerOfAListItemNeverSubmitsTheNext(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{Scope: "watch:1"}, 120, 30)
	m = press(t, selectRow(t, m, "auto-approve authors"), "enter", "a")
	m = typeIn(t, m, "rev-ann")
	m, late := stepOnly(t, m, keyMsg("enter")) // the item's form moves on through a command not run yet
	m = press(t, m, "esc", "a")                // that item cancelled, another one typed
	m = typeIn(t, m, "bob-rev")
	m = pump(t, m, settingsCmd(t, late)...).m
	if m.popup == nil || m.popup.list.form == nil {
		t.Fatal("the late answer of the cancelled item submitted the next one")
	}
	if len(m.draft) != 0 {
		t.Errorf("a late answer staged %+v", m.draft)
	}
}

// d on a role waits for the draft's roles too: the level whose cells go
// back must be the draft's.
func TestSettingsDOnARoleWaitsForTheDraftsRoles(t *testing.T) {
	m := testSettings(t, newFakeSettings(), newFakeRuntime(), SettingsOptions{}, 140, 30)
	m = press(t, selectRow(t, m, "spend level"), "enter", "j")
	m, staged := stepOnly(t, m, keyMsg("enter"))
	for queue := settingsCmd(t, staged); len(queue) > 0; {
		msg := queue[0]
		queue = queue[1:]
		if a, ok := msg.(settingsAsync); ok {
			if _, preview := a.msg.(settingsPreviewMsg); preview {
				continue // the draft's roles, held back
			}
		}
		var cmd tea.Cmd
		m, cmd = stepOnly(t, m, msg)
		queue = append(queue, settingsCmd(t, cmd)...)
	}
	m = press(t, selectRow(t, m, "claude-simplify"), "d")
	mustContain(t, viewOf(m), "wait: resolving the roles of the draft")
	if len(m.draft) != 1 {
		t.Errorf("d reset a role's cells at a level not yet resolved: %+v", m.draft)
	}
}

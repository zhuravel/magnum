package cli

// The settings screen's config side: internal/settings (the typed list of
// editable settings, the in-place editor and the checked save) adapted into
// tui.SettingsSource. The watch and repository settings are templates
// there; the screen gets one row per [[watch]] and per repository.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/launchd"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/settings"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// newSettingsSource is the settings screen's config source for c: the user
// config of c's layout over its --config file, with the registry acts opens
// for what the daemon runs and its restart mark.
func newSettingsSource(c *Context, acts *screenActions) tui.SettingsSource {
	return &settingsSource{layout: c.Layout, base: c.cfgPath, registry: acts.registry, daemon: acts.daemonState, now: time.Now}
}

// settingsSource adapts internal/settings for the screen. Load keeps the
// snapshot every later call works on (a save refuses a file that changed
// since) and the targets the screen's rows name.
type settingsSource struct {
	layout   paths.Layout
	base     string
	registry func() (*store.Store, error) // nil store: no registry (no restart mark, no pending restart)
	// daemon says whether a daemon runs and whether launchd runs it, so it
	// can exit to restart on a new file.
	daemon func(ctx context.Context) (running, supervised bool)
	now    func() time.Time

	mu      sync.Mutex
	snap    *settings.Snapshot
	targets map[string]settingsTargetOf
}

// settingsTargetOf is what a row of the screen edits.
type settingsTargetOf struct {
	target settings.Target
	field  settings.Field
}

var _ tui.SettingsSource = (*settingsSource)(nil)

// registry is the screens' registry, opened with their act deps.
func (s *screenActions) registry() (*store.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.deps()
	if err != nil {
		return nil, err
	}
	return d.Store, nil
}

// daemonState reports whether a daemon runs (its pidfile, without waking
// it) and whether it is launchd's job, as `magnum status` tells: launchd
// runs the daemon's own pid.
func (s *screenActions) daemonState(ctx context.Context) (running, supervised bool) {
	s.mu.Lock()
	d, err := s.deps()
	s.mu.Unlock()
	if err != nil || d.Running == nil {
		return false, false
	}
	pid, _ := d.Running()
	if pid <= 0 {
		return false, false
	}
	info, err := launchd.Status(ctx, d.Run, os.Getuid(), launchd.DefaultLabel)
	return true, err == nil && info.PID > 0 && info.PID == pid
}

func (s *settingsSource) Load(ctx context.Context) (*tui.SettingsData, error) {
	snap, err := settings.Load(s.layout, s.base)
	if err != nil {
		return nil, err
	}
	d := &tui.SettingsData{
		File: inspTilde(snap.Path), HistoryDir: inspTilde(settingsHistoryDir(s.layout)) + "/",
		Sections: settings.Sections(), Roles: settingsRoles(snap.Saved()), Warnings: snap.Config.Warnings(),
	}
	if st, err := s.registry(); err == nil && st != nil {
		d.RestartPending, _ = settings.RestartPending(ctx, st, snap)
		if r, ok := engine.ReadConfigRestartRefused(ctx, st); ok && r.SHA256 == hex.EncodeToString(snap.Sum[:]) {
			d.Warnings = append(d.Warnings, "the daemon refused to restart on this file at "+r.At.Local().Format("Jan 2 15:04")+
				": it fails the daemon's check (`magnum config` shows why)")
		}
	}
	targets := map[string]settingsTargetOf{}
	add := func(f settings.Field, t settings.Target, key, scope, inherits string) {
		targets[key] = settingsTargetOf{target: t, field: f}
		sf := settingsField(snap, f, t, key, scope)
		sf.Inherits = inherits
		d.Fields = append(d.Fields, sf)
	}
	watches, repos := snap.Watches(), snap.Repos()
	for _, f := range snap.Fields() {
		switch f.Scope {
		case settings.ScopeWatch:
			for _, w := range watches {
				scope := "watch:" + strconv.Itoa(w.Index+1)
				add(f, settings.Target{Field: f.ID, Watch: w.Index}, string(f.ID)+"@"+scope, scope, settingsInherits(f, -1, settings.Value{}))
			}
		case settings.ScopeRepo:
			for _, r := range repos {
				scope := "repo:" + r.Name
				add(f, settings.Target{Field: f.ID, Repo: r.Name}, string(f.ID)+"@"+scope, scope, settingsInherits(f, r.Watch, settings.Value{}))
			}
		default:
			add(f, settings.Target{Field: f.ID}, string(f.ID), "", settingsInherits(f, -1, snap.Value(settings.Target{Field: f.ID})))
		}
	}
	for _, w := range watches {
		d.Scopes = append(d.Scopes, tui.SettingsScope{ID: "watch:" + strconv.Itoa(w.Index+1), Watch: true, Index: w.Index + 1,
			Name: w.Owner, Repos: w.Include})
	}
	for _, r := range repos {
		d.Scopes = append(d.Scopes, tui.SettingsScope{ID: "repo:" + r.Name, Name: r.Name, NoBlock: !r.HasBlock})
	}
	s.mu.Lock()
	s.snap, s.targets = snap, targets
	s.mu.Unlock()
	return d, nil
}

// settingsInherits is the screen's target a setting takes its value from
// while unset, so its row follows a staged edit of that one ("inherit
// (burn)"): [pipeline] spend for a kind's spend that no layer sets (v is
// its value: a base config's [kinds.codex] spend = "low" inherits nothing),
// the daemon's interval for a watch's, the pipeline's cap and the covering
// watch's (watch, -1 for none) stakes and allowlist for a repository's.
func settingsInherits(f settings.Field, watch int, v settings.Value) string {
	inWatch := func(id string) string {
		if watch < 0 {
			return ""
		}
		return id + "@watch:" + strconv.Itoa(watch+1)
	}
	switch {
	case f.Scope == settings.ScopeKind:
		if v.State == settings.StateDefault && strings.HasPrefix(v.Effective, "inherit (") {
			return "pipeline.spend"
		}
		return ""
	case f.ID == "watch.own_min_rereview_interval":
		return "daemon.own_min_rereview_interval"
	case f.ID == "repo.max_simplifications":
		return "pipeline.max_simplifications"
	case f.ID == "repo.stakes":
		return inWatch("watch.stakes")
	case f.ID == "repo.auto_approve_authors":
		return inWatch("watch.auto_approve_authors")
	}
	return ""
}

// settingsHistoryDir is where the saves keep the files they replace.
func settingsHistoryDir(l paths.Layout) string { return filepath.Join(l.Data(), "config-history") }

// settingsField is one row of the screen: f for target t.
func settingsField(snap *settings.Snapshot, f settings.Field, t settings.Target, key, scope string) tui.SettingField {
	v := snap.Value(t)
	out := tui.SettingField{
		Target: key, Section: f.Section, Scope: scope, Label: f.Label, Key: snap.KeyText(t), Type: settingsType(f.Type),
		Options: snap.Options(t), Notes: settingsNotes(snap, f, t), Description: f.Description, Default: f.DefaultText,
		AppliesNow: f.Applies == settings.AppliesNow,
		Text:       v.Text, List: v.List, Value: v.Effective, Set: v.State == settings.StateSet, Line: v.Line, From: v.Source,
		Role: f.Role, Level: f.Level, Cell: f.Cell, Path: f.Key,
	}
	if f.Type.IsList() {
		out.ValidateList = func(l []string) error { return snap.Check(t, settings.DraftValue{List: l}) }
	} else {
		out.Validate = func(s string) error { return snap.Check(t, settings.DraftValue{Text: s}) }
	}
	return out
}

// settingsNotes are the notes on a setting's options: what each spend level
// starts from and how many roles its table sets, which roles judge.
func settingsNotes(snap *settings.Snapshot, f settings.Field, t settings.Target) map[string]string {
	notes := map[string]string{}
	switch {
	case f.Type == settings.TypeRoles:
		for _, r := range snap.Config.Roles {
			if r.Judge {
				notes[r.Name] = "judge"
			}
		}
	case f.ID == "pipeline.spend" || f.Scope == settings.ScopeKind:
		for _, level := range snap.Options(t) {
			note := "built-in"
			if !slices.Contains([]string{"low", "medium", "high"}, level) {
				note = "custom: starts from medium"
			}
			if n := len(snap.Config.SpendLevels[level]); n > 0 {
				note += ", then its table (" + textx.Count(n, "role", "roles") + ")"
			}
			notes[level] = note
		}
	}
	if len(notes) == 0 {
		return nil
	}
	return notes
}

// settingsType is a setting's type as the screen edits it.
func settingsType(t settings.Type) tui.SettingType {
	switch t {
	case settings.TypeEnum:
		return tui.SettingEnum
	case settings.TypeBool:
		return tui.SettingBool
	case settings.TypeDuration:
		return tui.SettingDuration
	case settings.TypeInt:
		return tui.SettingInt
	case settings.TypePercent:
		return tui.SettingPercent
	case settings.TypeLogins:
		return tui.SettingLogins
	case settings.TypeRepos:
		return tui.SettingRepos
	case settings.TypeRoles:
		return tui.SettingRoles
	case settings.TypeChecks:
		return tui.SettingList
	case settings.TypeModel:
		return tui.SettingModel
	case settings.TypeEffort:
		return tui.SettingEffort
	}
	return tui.SettingText
}

// settingsRoles are roles as the screen's grid shows them.
func settingsRoles(rows []settings.RoleRow) []tui.SettingsRole {
	out := make([]tui.SettingsRole, 0, len(rows))
	for _, r := range rows {
		out = append(out, tui.SettingsRole{Role: r.Role, Kind: r.Kind, Model: r.Model, First: r.Effort, Again: r.RereviewEffort, Level: r.Spend})
	}
	return out
}

// errNotLoaded is a call before Load read the file.
var errNotLoaded = errors.New("the config was not read yet")

// draft is the screen's draft in internal/settings' terms, on the snapshot
// Load read; afterRestart is true when a setting it changes applies only
// after the daemon restarts.
func (s *settingsSource) draft(d tui.SettingsDraft) (snap *settings.Snapshot, out settings.Draft, afterRestart bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap == nil {
		return nil, nil, false, errNotLoaded
	}
	out = settings.Draft{}
	for key, e := range d {
		t, ok := s.targets[key]
		if !ok {
			return nil, nil, false, fmt.Errorf("no setting %q in the file as read", key)
		}
		out[t.target] = settings.DraftValue{Text: e.Text, List: e.List, Default: e.Default}
		afterRestart = afterRestart || t.field.Applies == settings.AppliesAfterRestart
	}
	return s.snap, out, afterRestart, nil
}

func (s *settingsSource) Preview(_ context.Context, d tui.SettingsDraft) ([]tui.SettingsRole, error) {
	snap, draft, _, err := s.draft(d)
	if err != nil {
		return nil, err
	}
	rows, err := snap.Preview(draft)
	if err != nil {
		return nil, settingsReason(err)
	}
	return settingsRoles(rows), nil
}

// settingsReason is the loader's refusal without its "config <the layers
// read>: " prefix, whose paths would push the reason off the screen's
// line; any other error is itself.
func settingsReason(err error) error {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if inner := errors.Unwrap(e); inner != nil && strings.HasPrefix(e.Error(), "config ") {
			return inner
		}
	}
	return err
}

// settingsChanged marks a refusal of a file that changed on disk for the
// screen, which offers to read it again keeping the staged edits.
func settingsChanged(err error) error {
	if errors.Is(err, settings.ErrChanged) {
		return fmt.Errorf("%w: %w", tui.ErrSettingsChanged, err)
	}
	return err
}

// settingsPlanOf is a tui plan's Opaque: the save and whether it applies
// after a restart.
type settingsPlanOf struct {
	plan         *settings.Plan
	afterRestart bool
}

func (s *settingsSource) Plan(_ context.Context, d tui.SettingsDraft, notes bool) (*tui.SettingsPlan, error) {
	snap, draft, after, err := s.draft(d)
	if err != nil {
		return nil, err
	}
	p, err := snap.Plan(draft, notes, s.now())
	if err != nil {
		return nil, settingsChanged(settingsReason(err))
	}
	return settingsPlan(p, notes, 0, after), nil
}

// settingsPlan is p as the screen shows it.
func settingsPlan(p *settings.Plan, notes bool, restore int, afterRestart bool) *tui.SettingsPlan {
	out := &tui.SettingsPlan{Count: p.Count, Warnings: p.Warnings, Notes: notes, Restore: restore,
		Opaque: settingsPlanOf{plan: p, afterRestart: afterRestart}}
	for _, h := range p.Hunks {
		out.Hunks = append(out.Hunks, tui.SettingsHunk{Table: h.Table, Line: h.Line, Removed: h.Removed, Added: h.Added})
	}
	return out
}

func (s *settingsSource) Commit(_ context.Context, p *tui.SettingsPlan) (tui.SettingsSaved, error) {
	sp, ok := p.Opaque.(settingsPlanOf)
	if !ok {
		return tui.SettingsSaved{}, errors.New("not a plan of this config")
	}
	res, err := settings.Commit(sp.plan)
	if err != nil {
		return tui.SettingsSaved{}, settingsChanged(err)
	}
	return tui.SettingsSaved{History: inspTilde(res.History), Sum: hex.EncodeToString(res.Sum[:]), AfterRestart: sp.afterRestart, Opaque: res}, nil
}

func (s *settingsSource) RequestRestart(ctx context.Context, saved tui.SettingsSaved) (string, error) {
	res, ok := saved.Opaque.(settings.Result)
	if !ok {
		return "", errors.New("not a save of this config")
	}
	st, err := s.registry()
	if err != nil {
		return "", err
	}
	if st == nil {
		return "", errors.New("no registry to mark the restart in: run `magnum daemon-restart --when-idle`")
	}
	if err := settings.RequestRestart(ctx, st, res.Sum); err != nil {
		return "", err
	}
	switch running, supervised := s.daemon(ctx); {
	case !running:
		return "no daemon is running: it reads the saved file when it starts", nil
	case !supervised:
		return "the daemon is not launchd's job, so it cannot restart by itself: run `magnum daemon-restart --when-idle`", nil
	}
	return "the daemon restarts on the saved file at its first moment with nothing in flight", nil
}

// Restore is the save that puts the n-th kept version back, over the file
// Load read, or over the file on disk when it did not load (a hand edit
// broke it: the history mends it).
func (s *settingsSource) Restore(_ context.Context, n int) (*tui.SettingsPlan, error) {
	s.mu.Lock()
	snap := s.snap
	s.mu.Unlock()
	entries, err := settings.History(s.layout)
	if err != nil {
		return nil, err
	}
	if n < 1 || n > len(entries) {
		return nil, fmt.Errorf("no kept version %d: %s kept (magnum settings --history lists them)",
			n, textx.Count(len(entries), "version is", "versions are"))
	}
	var p *settings.Plan
	if snap != nil {
		p, err = snap.Restore(entries[n-1], s.now())
	} else {
		p, err = settings.RestoreFile(s.layout, s.base, entries[n-1])
	}
	if err != nil {
		return nil, settingsChanged(settingsReason(err))
	}
	return settingsPlan(p, false, n, true), nil
}

// settingsHistory prints the kept versions of the user config, newest
// first, numbered as --restore takes them; it reads no config, so a file a
// hand edit broke still lists them.
func settingsHistory(c *Context) error {
	entries, err := settings.History(c.Layout)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintf(c.Stdout, "no kept versions yet: a save keeps the file it replaces in %s\n", inspTilde(settingsHistoryDir(c.Layout)))
		return nil
	}
	for i, e := range entries {
		fmt.Fprintf(c.Stdout, "%3d  %s  %8s  %s  %s\n", i+1, e.Saved.Local().Format("2006-01-02 15:04"), settingsSize(e.Size),
			hex.EncodeToString(e.Sum[:])[:12], inspTilde(e.Path))
	}
	fmt.Fprintln(c.Stdout, "`magnum settings --restore N` shows the save that puts version N back")
	return nil
}

// settingsSize is a file size in KB.
func settingsSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
}

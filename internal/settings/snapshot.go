package settings

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/confedit"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
)

// State says whether the user config sets a setting.
type State int

// The states.
const (
	StateDefault State = iota // the file does not set it: the default (or what it inherits) applies
	StateSet                  // the file sets it
)

// Target is one setting of one scope instance: a global or concrete field
// alone, a watch field with its watch, a repository field with its
// repository.
type Target struct {
	Field FieldID
	Watch int    // ScopeWatch: the 0-based position among the user file's [[watch]] blocks
	Repo  string // ScopeRepo: owner/name, with or without a [[repo]] block yet
}

// Value is what the user config says for a target and what applies.
type Value struct {
	Text string   // what the file says, in the field's text form; "" when StateDefault
	List []string // a list field's entries as the file has them (StateSet)
	// Effective is what applies: the default (or what is inherited) when
	// StateDefault ("inherit (high)", "2h"), the resolved value when
	// StateSet.
	Effective string
	State     State
	Line      int    // the key's 1-based line in the user file (StateSet)
	Source    string // where Effective comes from: "your file, line 12", "built-in default", "[pipeline] spend", ...
}

// WatchInfo is one [[watch]] of the user file.
type WatchInfo struct {
	Index   int // its position among the user file's [[watch]] blocks
	Owner   string
	Label   string // "talkable · web, api +3"
	Include []string
	Line    int // its header's line
}

// RepoInfo is a repository the screen offers: a [[repo]] block of the user
// file, or a repository a watch's include names without one (the save
// creates a minimal block for it).
type RepoInfo struct {
	Name     string // owner/name
	HasBlock bool
	Line     int // the block's header line (HasBlock)
	Watch    int // the user file's [[watch]] that covers it; -1 for none, or a watch of the base file
}

// RoleRow is a role as it resolves after a restart: its kind, model, first
// and re-review effort, and the spend level it follows.
type RoleRow struct {
	Role, Kind, Model, Effort, RereviewEffort, Spend string
}

// Snapshot is the user config as the screen loaded it: its bytes and their
// hash (a save refuses a file that changed since), the scanned file, the
// resolved configuration, and the settings expanded for it.
type Snapshot struct {
	Path   string // the user config (paths.Layout.UserConfig)
	Bytes  []byte // its content; empty when it does not exist yet
	Sum    [32]byte
	Doc    *confedit.Doc
	Config *config.Config

	layout  paths.Layout
	base    string
	baseW   int // watches of the base configuration, before the user file's
	userW   int // the user file's [[watch]] blocks
	fields  []Field
	byID    map[FieldID]Field
	ordinal map[FieldID]int
}

// ErrChanged is a save over a config that changed on disk since the screen
// loaded it (an editor, another screen): nothing is written.
var ErrChanged = errors.New("settings: the config file changed on disk since it was loaded; reload it")

// Load reads the user config (layout.UserConfig; a missing file is an empty
// one) and resolves it over base as config.Load does (base: the --config
// file, "" for the built-in defaults or $MAGNUM_CONFIG). A file the editor
// does not model fails with confedit.ErrUnsupported, one magnum refuses with
// the loader's error.
func Load(layout paths.Layout, base string) (*Snapshot, error) {
	if layout.UserConfig == "" || !layout.Valid() {
		return nil, errors.New("settings: no user config path or data directory in the layout")
	}
	src, _, err := readConfig(layout.UserConfig)
	if err != nil {
		return nil, err
	}
	doc, err := confedit.Parse(src)
	if err != nil {
		return nil, fmt.Errorf("settings: %s: %w", layout.UserConfig, err)
	}
	cfg, err := loadConfig(layout, base, src)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Path: layout.UserConfig, Bytes: src, Sum: sha256.Sum256(src), Doc: doc, Config: cfg, layout: layout, base: base}
	s.userW = doc.Elements("watch")
	s.baseW = max(0, len(cfg.Watches)-s.userW)
	s.expand()
	return s, nil
}

// readConfig reads the user config: nil and false when it does not exist.
func readConfig(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return []byte{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("settings: %w", err)
	}
	return b, true, nil
}

// loadConfig resolves data as the user layer, as the daemon would read it.
func loadConfig(layout paths.Layout, base string, data []byte) (*config.Config, error) {
	if data == nil {
		data = []byte{}
	}
	return config.LoadWithOptions(layout, base, config.LoadOptions{UserData: data})
}

// Levels are the spend levels: low, medium, high, then the operator's own
// ([spend.<level>] tables) by name.
func (s *Snapshot) Levels() []string {
	out := []string{config.SpendLow, config.SpendMedium, config.SpendHigh}
	var own []string
	for l := range s.Config.SpendLevels {
		if !slices.Contains(out, l) {
			own = append(own, l)
		}
	}
	slices.Sort(own)
	return append(out, own...)
}

// kinds are the agent kinds the roles run, in [[role]] order.
func (s *Snapshot) kinds() []string {
	var out []string
	for _, r := range s.Config.AllRoles() {
		if k := r.AgentKind(); k != "" && !slices.Contains(out, k) {
			if _, ok := s.Config.KindSpec(k); ok {
				out = append(out, k)
			}
		}
	}
	return out
}

// expand builds the concrete fields: the grid per level, role and cell, the
// base model per role, the spend per kind; watch and repository fields stay
// templates.
func (s *Snapshot) expand() {
	roles := s.Config.AllRoles()
	cells := []string{CellModel, CellEffort, CellRereviewEffort}
	byCell := map[string]Field{}
	for _, f := range Fields() {
		if f.Scope == ScopeSpendLevel {
			byCell[f.Cell] = f
		}
	}
	var out []Field
	for _, f := range Fields() {
		switch f.Scope {
		case ScopeKind:
			for _, k := range s.kinds() {
				c := f
				c.ID, c.Kind, c.Label, c.Key = FieldID("kinds."+k+".spend"), k, k+" spend", "kinds."+k+".spend"
				out = append(out, c)
			}
		case ScopeSpendLevel:
			if f.Cell != CellModel {
				continue // the model template places the whole grid
			}
			for _, lvl := range s.Levels() {
				for _, r := range roles {
					for _, cell := range cells {
						c := byCell[cell]
						c.ID = FieldID("spend." + lvl + "." + r.Name + "." + cell)
						c.Level, c.Role = lvl, r.Name
						c.Label = r.Name + " " + strings.ReplaceAll(cell, "_", " ")
						c.Key = "spend." + lvl + "." + r.Name + "." + cell
						out = append(out, c)
					}
				}
			}
		case ScopeRole:
			for _, r := range roles {
				c := f
				c.ID, c.Role, c.Label, c.Key = FieldID("role."+r.Name+"."+f.Cell), r.Name, r.Name+" base model", "role."+r.Name+"."+f.Cell
				out = append(out, c)
			}
		default:
			out = append(out, f)
		}
	}
	s.fields = out
	s.byID = make(map[FieldID]Field, len(out))
	s.ordinal = make(map[FieldID]int, len(out))
	for i, f := range out {
		s.byID[f.ID], s.ordinal[f.ID] = f, i
	}
}

// Fields are the settings of this configuration, in screen order: the grid
// for every level, role and cell, the base model per role and the spend per
// kind concrete, the watch and repository fields as templates (a Target
// picks the instance).
func (s *Snapshot) Fields() []Field { return slices.Clone(s.fields) }

// Field is the setting id names.
func (s *Snapshot) Field(id FieldID) (Field, bool) {
	f, ok := s.byID[id]
	return f, ok
}

// Watches are the user file's [[watch]] blocks.
func (s *Snapshot) Watches() []WatchInfo {
	out := make([]WatchInfo, 0, s.userW)
	for i := range s.userW {
		w := s.Config.Watches[s.baseW+i]
		line := 0
		if t, ok := s.Doc.Watch(i); ok {
			line, _ = s.Doc.HeaderLine(t)
		}
		out = append(out, WatchInfo{Index: i, Owner: w.Owner, Label: watchLabel(w), Include: slices.Clone(w.Include), Line: line})
	}
	return out
}

func watchLabel(w config.Watch) string {
	inc := w.Include
	if len(inc) == 0 {
		inc = []string{"*"}
	}
	l := w.Owner + " · " + strings.Join(inc[:min(2, len(inc))], ", ")
	if len(inc) > 2 {
		l += " +" + strconv.Itoa(len(inc)-2)
	}
	return l
}

// Repos are the repositories the screen offers, by name: each [[repo]]
// block of the user file, and each repository a watch's include names
// (without a glob) that the watch covers, with or without a block.
func (s *Snapshot) Repos() []RepoInfo {
	var out []RepoInfo
	seen := map[string]int{}
	add := func(r RepoInfo) {
		if i, ok := seen[strings.ToLower(r.Name)]; ok {
			if r.HasBlock {
				out[i].HasBlock, out[i].Line = true, r.Line
			}
			return
		}
		seen[strings.ToLower(r.Name)] = len(out)
		out = append(out, r)
	}
	for i := range s.Doc.Elements("repo") {
		t := confedit.Table{Path: []string{"repo"}, Index: i}
		v, ok := s.Doc.Get(confedit.Key{Table: t, Path: []string{"repo"}})
		name, _ := v.Data.(string)
		if !ok || name == "" {
			continue
		}
		line, _ := s.Doc.HeaderLine(t)
		add(RepoInfo{Name: name, HasBlock: true, Line: line, Watch: s.userWatchOf(name)})
	}
	for wi := range s.Config.Watches {
		w := &s.Config.Watches[wi]
		for _, inc := range w.Include {
			if inc == "" || strings.ContainsAny(inc, "*?[\\") {
				continue
			}
			full := w.Owner + "/" + inc
			if s.Config.WatchFor(full) != w || s.baseRepo(full) {
				continue
			}
			add(RepoInfo{Name: full, Watch: s.userWatchOf(full)})
		}
	}
	slices.SortFunc(out, func(a, b RepoInfo) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

// baseRepo reports whether repository full has a [[repo]] block in the
// base configuration and none in the user file: a block of the user file
// would configure it twice, so the screen leaves it to a hand edit.
func (s *Snapshot) baseRepo(full string) bool {
	_, inUser := s.Doc.Repo(full)
	return !inUser && s.Config.RepoFor(full) != nil
}

// userWatchOf is the user file's [[watch]] that covers a repository, -1 for
// none (or a watch of the base configuration).
func (s *Snapshot) userWatchOf(full string) int {
	w := s.Config.WatchFor(full)
	for i := range s.userW {
		if w == &s.Config.Watches[s.baseW+i] {
			return i
		}
	}
	return -1
}

// watch is the resolved [[watch]] at the user file's position i (nil when
// there is none).
func (s *Snapshot) watch(i int) *config.Watch {
	if i < 0 || i >= s.userW {
		return nil
	}
	return &s.Config.Watches[s.baseW+i]
}

// confKey is where target t lives in the user file: its key, and the table
// it needs created first (a [[role]] or [[repo]] block not in the file yet:
// create names the element's identifying key, name or repo).
func (s *Snapshot) confKey(f Field, t Target) (k confedit.Key, create *confedit.Change, err error) {
	switch f.Scope {
	case ScopeGlobal:
		section, key, _ := strings.Cut(f.Key, ".")
		return confedit.Key{Table: confedit.Section(section), Path: []string{key}}, nil, nil
	case ScopeKind:
		return confedit.Key{Table: confedit.Section("kinds", f.Kind), Path: []string{"spend"}}, nil, nil
	case ScopeSpendLevel:
		return confedit.Key{Table: confedit.Section("spend", f.Level), Path: []string{f.Role, f.Cell}}, nil, nil
	case ScopeRole:
		if n := s.roleBlocks(f.Role); n > 1 {
			return confedit.Key{}, nil, fmt.Errorf("settings: %d [[role]] blocks of your config name %s, and the loader merges them "+
				"(the last one's keys win): edit them by hand", n, f.Role)
		}
		tbl, ok := s.Doc.Role(f.Role)
		if !ok {
			tbl = confedit.Table{Path: []string{"role"}, Index: s.Doc.Elements("role")}
			create = &confedit.Change{Key: confedit.Key{Table: tbl, Path: []string{"name"}}, Value: f.Role}
		}
		return confedit.Key{Table: tbl, Path: []string{f.Cell}}, create, nil
	case ScopeWatch:
		tbl, ok := s.Doc.Watch(t.Watch)
		if !ok {
			return confedit.Key{}, nil, fmt.Errorf("settings: your config has no watch %d", t.Watch+1)
		}
		_, key, _ := strings.Cut(f.Key, ".")
		return confedit.Key{Table: tbl, Path: []string{key}}, nil, nil
	case ScopeRepo:
		owner, name, ok := strings.Cut(t.Repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return confedit.Key{}, nil, fmt.Errorf("settings: repository %q: want owner/name", t.Repo)
		}
		tbl, ok := s.Doc.Repo(t.Repo)
		if !ok && s.baseRepo(t.Repo) {
			return confedit.Key{}, nil, fmt.Errorf("settings: %s's [[repo]] block is in the base config (%s); edit it there", t.Repo, s.base)
		}
		if !ok {
			tbl = confedit.Table{Path: []string{"repo"}, Index: s.Doc.Elements("repo")}
			create = &confedit.Change{Key: confedit.Key{Table: tbl, Path: []string{"repo"}}, Value: t.Repo}
		}
		_, key, _ := strings.Cut(f.Key, ".")
		return confedit.Key{Table: tbl, Path: []string{key}}, create, nil
	}
	return confedit.Key{}, nil, fmt.Errorf("settings: field %s has no key", f.ID)
}

// roleBlocks is how many [[role]] blocks of the user file name a role.
func (s *Snapshot) roleBlocks(name string) int {
	n := 0
	for i := range s.Doc.Elements("role") {
		v, _ := s.Doc.Get(confedit.Key{Table: confedit.Table{Path: []string{"role"}, Index: i}, Path: []string{"name"}})
		if v.Data == name {
			n++
		}
	}
	return n
}

// Value is what the user config says for t and what applies. A role that
// several [[role]] blocks of the file name is not edited here (Check and
// Plan refuse it): its Source says so.
func (s *Snapshot) Value(t Target) Value {
	f, ok := s.byID[t.Field]
	if !ok {
		return Value{}
	}
	var v Value
	if f.Scope == ScopeRole {
		if n := s.roleBlocks(f.Role); n > 1 {
			r, _ := s.Config.BaseRole(f.Role)
			return Value{Effective: cmp.Or(r.Model, s.kindModel(f.Role), "the CLI's default"), State: StateSet,
				Source: fmt.Sprintf("%d [[role]] blocks of your file name %s (the last one's keys win): edit them by hand", n, f.Role)}
		}
	}
	if k, create, err := s.confKey(f, t); err == nil && create == nil {
		if got, ok := s.Doc.Get(k); ok {
			v = Value{Text: textOf(got.Data), List: listOf(got.Data), State: StateSet, Line: got.Line,
				Source: "your file, line " + strconv.Itoa(got.Line)}
		}
	}
	v.Effective = s.effective(f, t, v)
	if v.State == StateDefault {
		v.Source = s.defaultSource(f)
	}
	return v
}

// fromBase is the Source of a value the user file does not set but a
// layer under it does: the base configuration, else the built-in defaults.
func (s *Snapshot) fromBase(what string) string {
	if s.base != "" {
		return "the base config (" + path.Base(s.base) + ")" + what
	}
	return "the built-in defaults" + what
}

// textOf is a decoded value in a field's text form.
func textOf(data any) string {
	switch x := data.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		return strings.Join(listOf(x), ", ")
	case nil:
		return ""
	}
	lit, _ := confedit.Literal(data)
	return lit
}

func listOf(data any) []string {
	arr, ok := data.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, el := range arr {
		out = append(out, textOf(el))
	}
	return out
}

func listText(l []string, none string) string {
	if len(l) == 0 {
		return none
	}
	return strings.Join(l, ", ")
}

// durationText is a duration as config.toml writes it ("15m", "1h30m",
// "2d"; "0" for none).
func durationText(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	var b strings.Builder
	for _, u := range []struct {
		unit time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if n := d / u.unit; n > 0 {
			b.WriteString(strconv.FormatInt(int64(n), 10) + u.name)
			d %= u.unit
		}
	}
	if d > 0 {
		b.WriteString(d.String())
	}
	return b.String()
}

func floatText(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// role is the resolved role named name.
func (s *Snapshot) role(name string) (config.Role, bool) {
	for _, r := range s.Config.AllRoles() {
		if r.Name == name {
			return r, true
		}
	}
	return config.Role{}, false
}

// effective is what applies for t (v: what the file says).
func (s *Snapshot) effective(f Field, t Target, v Value) string {
	c := s.Config
	inherit := func(what string) string {
		if v.State == StateSet {
			return v.Text
		}
		return "inherit (" + what + ")"
	}
	switch f.Scope {
	case ScopeKind:
		if v.State == StateSet {
			return v.Text
		}
		if k, _ := c.KindSpec(f.Kind); k.Spend != "" {
			return k.Spend // a layer under the user file sets it (defaultSource says which)
		}
		return inherit(cmp.Or(c.Pipeline.Spend, config.SpendMedium))
	case ScopeSpendLevel:
		if v.State == StateSet {
			return v.Text
		}
		if b, ok := s.baseCell(f); ok {
			return b
		}
		for _, r := range c.RolesAtLevel(f.Level) {
			if r.Name == f.Role {
				return inherit(cmp.Or(cellOf(r, f.Cell, s), "-"))
			}
		}
		return "inherit"
	case ScopeRole:
		if v.State == StateSet {
			return v.Text
		}
		r, _ := c.BaseRole(f.Role)
		return cmp.Or(r.Model, s.kindModel(f.Role), "the CLI's default")
	case ScopeWatch:
		return s.watchEffective(f, t, v)
	case ScopeRepo:
		return s.repoEffective(f, t, v)
	}
	d, p := c.Daemon, c.Pipeline
	switch f.ID {
	case "pipeline.spend":
		return cmp.Or(p.Spend, config.SpendMedium)
	case "usage.codex_soft":
		return floatText(c.Usage.CodexSoft)
	case "usage.codex_hard":
		return floatText(c.Usage.CodexHard)
	case "daemon.quiet_hours":
		return cmp.Or(d.QuietHours, "none")
	case "daemon.push_quiet_period":
		return durationText(d.PushQuietPeriod.Duration)
	case "daemon.min_rereview_interval":
		return durationText(d.MinRereviewInterval.Duration)
	case "daemon.own_min_rereview_interval":
		if d.OwnMinRereviewInterval.Duration == 0 {
			return "0 (the re-review interval, " + durationText(d.MinRereviewInterval.Duration) + ")"
		}
		return durationText(d.OwnMinRereviewInterval.Duration)
	case "pipeline.status_comment":
		return strconv.FormatBool(p.StatusComment)
	case "pipeline.whiteboard":
		return strconv.FormatBool(p.Whiteboard)
	case "pipeline.judge_fallback":
		return cmp.Or(c.JudgeFallbackName(nil), "none") // the resolved one: the built-in default is off where it does not fit
	case "pipeline.fix_cycles":
		return strconv.Itoa(p.FixCycles)
	case "pipeline.autofix":
		return cmp.Or(p.Autofix, config.AutofixNever)
	case "pipeline.max_simplifications":
		return strconv.Itoa(p.MaxSimplifications)
	case "daemon.park_idle_after":
		return durationText(d.ParkIdleAfter.Duration)
	case "pipeline.judge_fresh_after":
		return durationText(p.JudgeFreshAfter.Duration)
	case "daemon.max_concurrent_reviews":
		return strconv.Itoa(d.MaxConcurrentReviews)
	case "daemon.max_total_working_codex":
		return strconv.Itoa(d.MaxTotalWorkingCodex)
	}
	return v.Text
}

// baseCell is a level table's cell a layer under the user file sets (the
// merged [spend.<level>] tables hold it, the user file does not).
func (s *Snapshot) baseCell(f Field) (string, bool) {
	sr, ok := s.Config.SpendLevels[f.Level][f.Role]
	if !ok {
		return "", false
	}
	var p *string
	switch f.Cell {
	case CellModel:
		p = sr.Model
	case CellEffort:
		p = sr.Effort
	case CellRereviewEffort:
		p = sr.RereviewEffort
	}
	if p == nil {
		return "", false
	}
	return *p, true
}

func cellOf(r config.Role, cell string, s *Snapshot) string {
	switch cell {
	case CellModel:
		k, _ := s.Config.KindSpec(r.AgentKind())
		return cmp.Or(r.Model, k.DefaultModel)
	case CellEffort:
		return r.Effort
	}
	return r.RereviewEffort
}

// kindModel is the default model of the kind a role runs.
func (s *Snapshot) kindModel(name string) string {
	r, ok := s.role(name)
	if !ok {
		return ""
	}
	k, _ := s.Config.KindSpec(r.AgentKind())
	return k.DefaultModel
}

func (s *Snapshot) watchEffective(f Field, t Target, v Value) string {
	w := s.watch(t.Watch)
	if w == nil {
		return v.Text
	}
	switch f.ID {
	case "watch.roles":
		var names []string
		for _, r := range s.Config.RolesFor(w) {
			names = append(names, r.Name)
		}
		return listText(names, "none")
	case "watch.auto_approve":
		return listText(w.AutoApprove, "none")
	case "watch.auto_approve_authors":
		return listText(w.AutoApproveAuthors, "every author")
	case "watch.auto_approve_skip_authors":
		return listText(w.AutoApproveSkipAuthors, "none")
	case "watch.manual_repos":
		return listText(w.ManualRepos, "none")
	case "watch.include_drafts":
		return strconv.FormatBool(w.DraftsIncluded())
	case "watch.stakes":
		return cmp.Or(w.Stakes, "none")
	case "watch.own_min_rereview_interval":
		if w.OwnMinRereviewInterval.Duration == 0 {
			return "inherit (" + durationText(s.Config.Daemon.OwnMinRereviewInterval.Duration) + ")"
		}
		return durationText(w.OwnMinRereviewInterval.Duration)
	}
	return v.Text
}

func (s *Snapshot) repoEffective(f Field, t Target, v Value) string {
	r := s.Config.RepoFor(t.Repo)
	var rr config.Repo
	if r != nil {
		rr = *r
	}
	w := s.Config.WatchFor(t.Repo)
	switch f.ID {
	case "repo.stakes":
		if rr.Stakes != "" {
			return rr.Stakes
		}
		ws := "none"
		if w != nil && w.Stakes != "" {
			ws = w.Stakes
		}
		return "inherit (" + ws + ")"
	case "repo.max_simplifications":
		if rr.MaxSimplifications > 0 {
			return strconv.Itoa(rr.MaxSimplifications)
		}
		return "inherit (" + strconv.Itoa(s.Config.Pipeline.MaxSimplifications) + ")"
	case "repo.auto_approve_authors":
		if len(rr.AutoApproveAuthors) > 0 {
			return listText(rr.AutoApproveAuthors, "")
		}
		var wl []string
		if w != nil {
			wl = w.AutoApproveAuthors
		}
		return "inherit (" + listText(wl, "every author") + ")"
	case "repo.auto_approve_skip_authors":
		return listText(rr.AutoApproveSkipAuthors, "none")
	case "repo.allow_bot_authors":
		return listText(rr.AllowBotAuthors, "none")
	case "repo.required_checks":
		return listText(rr.RequiredChecks, "GitHub's required checks")
	}
	return v.Text
}

// defaultSource says where a default comes from, from the resolved layers.
func (s *Snapshot) defaultSource(f Field) string {
	switch f.Scope {
	case ScopeKind:
		if k, _ := s.Config.KindSpec(f.Kind); k.Spend != "" {
			return s.fromBase(" [kinds." + f.Kind + "] spend")
		}
		return "[pipeline] spend"
	case ScopeSpendLevel:
		if _, ok := s.baseCell(f); ok {
			return s.fromBase(" [spend." + f.Level + "] table")
		}
		return "not in a [spend." + f.Level + "] table: the role's own, after the level's notch"
	case ScopeRole:
		if s.base != "" {
			return "the base config's [[role]] block, the built-in role, else the kind's default model"
		}
		return "the built-in role, else the kind's default model"
	case ScopeRepo:
		return "the watch's, or the built-in default"
	}
	if s.base != "" {
		return "the base config (" + path.Base(s.base) + ") or the built-in default"
	}
	return "built-in default"
}

// KeyText names t's key as the file spells it: "[pipeline] spend",
// "[spend.high].claude-simplify.model", "[[watch]] 1 (talkable) · roles",
// "[[repo]] talkable/web · stakes", "[[role]] claude-review · model".
func (s *Snapshot) KeyText(t Target) string {
	f, ok := s.byID[t.Field]
	if !ok {
		return string(t.Field)
	}
	switch f.Scope {
	case ScopeKind:
		return "[kinds." + f.Kind + "] spend"
	case ScopeSpendLevel:
		return "[spend." + f.Level + "]." + f.Role + "." + f.Cell
	case ScopeRole:
		return "[[role]] " + f.Role + " · " + f.Cell
	case ScopeWatch:
		_, key, _ := strings.Cut(f.Key, ".")
		owner := ""
		if w := s.watch(t.Watch); w != nil {
			owner = " (" + w.Owner + ")"
		}
		return "[[watch]] " + strconv.Itoa(t.Watch+1) + owner + " · " + key
	case ScopeRepo:
		_, key, _ := strings.Cut(f.Key, ".")
		return "[[repo]] " + t.Repo + " · " + key
	}
	section, key, _ := strings.Cut(f.Key, ".")
	return "[" + section + "] " + key
}

// models are the models the configuration names anywhere: the roles, the
// level tables, the kinds' default and fallback models.
func (s *Snapshot) models() []string {
	set := map[string]bool{}
	for _, r := range s.Config.AllRoles() {
		set[r.Model] = true
	}
	for _, roles := range s.Config.SpendLevels {
		for _, sr := range roles {
			if sr.Model != nil {
				set[*sr.Model] = true
			}
		}
	}
	for _, k := range s.kinds() {
		spec, _ := s.Config.KindSpec(k)
		set[spec.DefaultModel] = true
		for _, m := range spec.FallbackModels {
			set[m] = true
		}
	}
	for _, r := range config.DefaultRoles() {
		set[r.Model] = true
	}
	delete(set, "")
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// efforts are Efforts and any other effort the configuration names.
func (s *Snapshot) efforts() []string {
	out := slices.Clone(Efforts)
	for _, r := range s.Config.AllRoles() {
		for _, e := range []string{r.Effort, r.RereviewEffort} {
			if e != "" && !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	return out
}

// Options are the values a target offers: an enum's members, the spend
// levels, the round roles, the judges, the models and efforts the
// configuration names, a watch's repositories ("" is a judge_fallback of
// none).
func (s *Snapshot) Options(t Target) []string {
	f, ok := s.byID[t.Field]
	if !ok {
		return nil
	}
	switch {
	case f.ID == "pipeline.spend" || f.Scope == ScopeKind:
		return s.Levels()
	case f.Type == TypeModel:
		return s.models()
	case f.Type == TypeEffort:
		return s.efforts()
	case f.ID == "watch.roles":
		var out []string
		for _, r := range s.Config.AllRoles() {
			if !r.IsFix() && !r.IsWhiteboard() {
				out = append(out, r.Name)
			}
		}
		return out
	case f.ID == "pipeline.judge_fallback":
		out := []string{""}
		for _, r := range s.Config.Judges() {
			out = append(out, r.Name)
		}
		return out
	case f.ID == "watch.auto_approve" || f.ID == "watch.manual_repos":
		var out []string
		if f.ID == "watch.auto_approve" {
			out = append(out, "*")
		}
		if w := s.watch(t.Watch); w != nil {
			for _, inc := range w.Include {
				if !strings.ContainsAny(inc, "*?[\\") {
					out = append(out, inc)
				}
			}
		}
		return out
	}
	return slices.Clone(f.Options)
}

// Check is t's own validation of a draft value: the field's check, an
// enum's members, the round roles of a roles list. Whether the whole file
// still loads is Plan's check.
func (s *Snapshot) Check(t Target, v DraftValue) error {
	f, ok := s.byID[t.Field]
	if !ok {
		return fmt.Errorf("settings: no setting %s", t.Field)
	}
	if v.Default {
		return nil
	}
	if f.Type.IsList() {
		list := draftList(v)
		if f.ValidateList != nil {
			if err := f.ValidateList(list); err != nil {
				return err
			}
		}
		if f.Type == TypeRoles {
			opts := s.Options(t)
			for _, r := range list {
				if !slices.Contains(opts, r) {
					return fmt.Errorf("%q is not a round role (%s)", r, strings.Join(opts, ", "))
				}
			}
		}
		return nil
	}
	if f.Scope == ScopeRole {
		if n := s.roleBlocks(f.Role); n > 1 {
			return fmt.Errorf("%d [[role]] blocks of your config name %s: edit them by hand", n, f.Role)
		}
	}
	switch f.Type {
	case TypeEnum, TypeBool:
		if opts := s.Options(t); !slices.Contains(opts, v.Text) {
			return fmt.Errorf("must be one of %s", strings.Join(slices.DeleteFunc(opts, func(o string) bool { return o == "" }), ", "))
		}
		return nil
	}
	if f.Validate != nil {
		return f.Validate(v.Text)
	}
	return nil
}

// draftList is a list draft's entries: List, else Text split at commas.
func draftList(v DraftValue) []string {
	if v.List != nil {
		return v.List
	}
	out := []string{} // a cleared list is [], not nothing
	for part := range strings.SplitSeq(v.Text, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

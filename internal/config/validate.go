package config

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"
	"unicode"
)

// Warnings lists human-readable problems that do not make the config
// invalid: zero [[identity]] or zero [[watch]] blocks are valid (the
// built-in defaults have neither; they live in the user's ~/.config/magnum/config.toml), but
// magnum then cannot post or reviews nothing. Print them from `magnum
// doctor` and `magnum config`.
func (c *Config) Warnings() []string {
	var out []string
	if len(c.Identities) == 0 {
		out = append(out, "no [[identity]] configured: magnum cannot poll GitHub or post reviews (declare identities in ~/.config/magnum/config.toml: `magnum init` writes one; examples in config.full.example.toml)")
	}
	if len(c.Watches) == 0 {
		out = append(out, "no [[watch]] configured, magnum reviews nothing (declare watches in ~/.config/magnum/config.toml: `magnum init` writes one; examples in config.full.example.toml)")
	}
	return out
}

// Validate checks cross references and required fields.
func (c *Config) Validate() error {
	var errs []error
	for _, msg := range c.Board.validate() {
		errs = append(errs, errors.New(msg))
	}
	ids := map[string]Identity{}
	for _, id := range c.Identities {
		if id.Name == "" || id.Login == "" {
			errs = append(errs, fmt.Errorf("identity needs name and login: %+v", id))
			continue
		}
		if _, dup := ids[id.Name]; dup {
			errs = append(errs, fmt.Errorf("identity %q is declared twice", id.Name))
		}
		switch id.Kind {
		case "gh":
		case "app":
			if id.AppID == 0 || id.InstallationID == 0 || (id.PrivateKeyEnv == "" && id.PrivateKeyFile == "") {
				errs = append(errs, fmt.Errorf("identity %s: app needs app_id, installation_id and private_key_file (or private_key_env)", id.Name))
			}
			if id.ClientID == "" {
				errs = append(errs, fmt.Errorf("identity %s: app needs client_id (JWT iss)", id.Name))
			}
		default:
			errs = append(errs, fmt.Errorf("identity %s: kind must be gh or app", id.Name))
		}
		errs = append(errs, validateVerdicts("identity "+id.Name, id.NoFindingsEvent, id.BlockingEvent)...)
		ids[id.Name] = id
	}
	for _, w := range c.Watches {
		if w.Owner == "" || len(w.Include) == 0 {
			errs = append(errs, fmt.Errorf("watch %q needs owner and include", w.Owner))
		}
		for _, msg := range trackerProblems("watch "+w.Owner, w.Trackers) {
			errs = append(errs, errors.New(msg))
		}
		if _, ok := ids[w.Identity]; !ok {
			errs = append(errs, fmt.Errorf("watch %s: unknown identity %q", w.Owner, w.Identity))
		}
		if _, ok := ids[w.PollIdentity]; !ok {
			errs = append(errs, fmt.Errorf("watch %s: unknown poll_identity %q", w.Owner, w.PollIdentity))
		}
		for _, list := range []struct {
			key   string
			globs []string
		}{{"include", w.Include}, {"exclude", w.Exclude}} {
			for _, g := range list.globs {
				// Watch.Matches ignores errors, so a malformed glob would
				// silently match nothing; Match reports it even for "".
				if _, err := path.Match(g, ""); errors.Is(err, path.ErrBadPattern) {
					errs = append(errs, fmt.Errorf("watch %s: %s pattern %q: %w", w.Owner, list.key, g, err))
				}
			}
		}
		for _, g := range w.SkipPaths {
			if err := ValidatePathGlob(g); err != nil {
				errs = append(errs, fmt.Errorf("watch %s: skip_paths pattern %q: %w", w.Owner, g, err))
			}
		}
		errs = append(errs, validateBurst("watch "+w.Owner+": ", w.BurstQuietPeriod, w.BurstPushes, w.BurstWindow)...)
		errs = append(errs, validateTrivialDeltas("watch "+w.Owner+": ", w.SkipTrivialDeltas)...)
		errs = append(errs, validateRereviewDelta("watch "+w.Owner+": ", w.RereviewMinLines, w.RereviewMaxWait)...)
		for _, t := range w.RequestTeams {
			if strings.TrimSpace(t) == "" || strings.ContainsAny(t, "/ @") {
				errs = append(errs, fmt.Errorf("watch %s: request_teams entry %q must be a team slug (no owner, no @)", w.Owner, t))
			}
		}
	}
	pools := map[string]bool{}
	for _, p := range c.Pools {
		if key := strings.ToLower(p.Repo); pools[key] {
			errs = append(errs, fmt.Errorf("pool %s: configured twice", p.Repo))
		} else {
			pools[key] = true
		}
		if !strings.Contains(p.Repo, "/") || p.MainClone == "" {
			errs = append(errs, fmt.Errorf("pool %q needs repo owner/name and main_clone", p.Repo))
		}
		if !strings.Contains(p.SlotName, "{n}") || !strings.Contains(p.SlotPath, "{n}") {
			errs = append(errs, fmt.Errorf("pool %s: slot_name and slot_path must contain {n}", p.Repo))
		}
		if p.Min < 0 || p.Max < p.Min || p.Max == 0 {
			errs = append(errs, fmt.Errorf("pool %s: need 0 <= min <= max, max > 0", p.Repo))
		}
		for _, d := range p.Databases {
			if base, ok := strings.CutSuffix(d, DatabaseSuffix); !ok || base == "" || strings.ContainsAny(base, "{}") {
				errs = append(errs, fmt.Errorf("pool %s: database template %q must be <name>%s with no other placeholder "+
					"(magnum lists, and drops, a slot's databases by that suffix)", p.Repo, d, DatabaseSuffix))
			}
		}
		errs = append(errs, validateCopyFiles("pool "+p.Repo, p.CopyFiles)...)
		errs = append(errs, validateReadiness("pool "+p.Repo, p.Prepare, p.Ready, p.ReadyTimeout)...)
		if c.WatchFor(p.Repo) == nil {
			errs = append(errs, fmt.Errorf("pool %s: no [[watch]] covers it", p.Repo))
		}
	}
	errs = append(errs, c.validateRepos()...)
	errs = append(errs, c.validatePipeline()...)
	if c.Daemon.MaxConcurrentReviews < 1 {
		errs = append(errs, errors.New("daemon.max_concurrent_reviews must be >= 1"))
	}
	if !filepath.IsAbs(c.Herdr.Socket) {
		errs = append(errs, fmt.Errorf("herdr.socket must be absolute after expansion: %s", c.Herdr.Socket))
	}
	errs = append(errs, validateQuietHours(c.Daemon.QuietHours)...)
	errs = append(errs, validateKeep("daemon.keep_events", c.Daemon.KeepEvents)...)
	errs = append(errs, validateKeep("daemon.keep_requests", c.Daemon.KeepRequests)...)
	if c.Daemon.MaxRoundRestarts < 0 {
		errs = append(errs, errors.New("daemon.max_round_restarts must be >= 0"))
	}
	errs = append(errs, validateBurst("daemon.", c.Daemon.BurstQuietPeriod, &c.Daemon.BurstPushes, c.Daemon.BurstWindow)...)
	errs = append(errs, validateTrivialDeltas("daemon.", c.Daemon.SkipTrivialDeltas)...)
	errs = append(errs, validateRereviewDelta("daemon.", &c.Daemon.RereviewMinLines, c.Daemon.RereviewMaxWait)...)
	if c.Daemon.RequestDebounce.Duration < 0 {
		errs = append(errs, errors.New("daemon.request_debounce must not be negative"))
	}
	if c.Daemon.ModelLimitCooldown.Duration < time.Minute {
		errs = append(errs, fmt.Errorf("daemon.model_limit_cooldown must be at least 1m, got %s", c.Daemon.ModelLimitCooldown.Duration))
	}
	if p := c.Daemon.ParkIdleAfter.Duration; p != 0 && p < time.Minute {
		errs = append(errs, fmt.Errorf("daemon.park_idle_after must be 0 (never) or at least 1m, got %s", p))
	}
	errs = append(errs, c.validateUsage()...)
	errs = append(errs, c.legacyErrs...)
	switch c.GitHub.Transport {
	case "gh", "direct":
	default:
		errs = append(errs, fmt.Errorf("github.transport must be gh or direct, got %q", c.GitHub.Transport))
	}
	switch c.Terminal.Icons {
	case "", "unicode", "nerd", "ascii":
	default:
		errs = append(errs, fmt.Errorf("terminal.icons must be unicode, nerd or ascii, got %q", c.Terminal.Icons))
	}
	return errors.Join(errs...)
}

// validateUsage checks [usage]: each threshold is 0 (off) or a percentage
// in (0, 100], and the soft one lies below the hard one when both are on.
func (c *Config) validateUsage() []error {
	var errs []error
	u := c.Usage
	for _, x := range []struct {
		key string
		v   float64
	}{{"usage.codex_soft", u.CodexSoft}, {"usage.codex_hard", u.CodexHard}} {
		if x.v < 0 || x.v > 100 {
			errs = append(errs, fmt.Errorf("%s must be a percentage between 0 (off) and 100, got %g", x.key, x.v))
		}
	}
	if u.CodexSoft > 0 && u.CodexHard > 0 && u.CodexSoft >= u.CodexHard {
		errs = append(errs, fmt.Errorf("usage.codex_soft (%g) must be below usage.codex_hard (%g)", u.CodexSoft, u.CodexHard))
	}
	return errs
}

// validateBurst checks the burst keys of [daemon] or a [[watch]] (prefix
// names where): no negative duration or count.
func validateBurst(prefix string, quiet Duration, pushes *int, window Duration) []error {
	var errs []error
	if quiet.Duration < 0 {
		errs = append(errs, fmt.Errorf("%sburst_quiet_period must not be negative", prefix))
	}
	if pushes != nil && *pushes < 0 {
		errs = append(errs, fmt.Errorf("%sburst_pushes must be >= 0", prefix))
	}
	if window.Duration < 0 {
		errs = append(errs, fmt.Errorf("%sburst_window must not be negative", prefix))
	}
	return errs
}

// validateRereviewDelta checks rereview_min_lines and rereview_max_wait of
// [daemon] or a [[watch]] (prefix names where): neither may be negative.
func validateRereviewDelta(prefix string, minLines *int, maxWait Duration) []error {
	var errs []error
	if minLines != nil && *minLines < 0 {
		errs = append(errs, fmt.Errorf("%srereview_min_lines must be >= 0 (0 = no threshold)", prefix))
	}
	if maxWait.Duration < 0 {
		errs = append(errs, fmt.Errorf("%srereview_max_wait must not be negative", prefix))
	}
	return errs
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateRepos checks the [[repo]] blocks: owner/name covered by a watch,
// not a pool repository, no duplicates, shell-safe env keys, copy_files
// that stay inside the clone and well-formed required_checks globs.
func (c *Config) validateRepos() []error {
	var errs []error
	seen := map[string]bool{}
	for _, r := range c.Repos {
		owner, name, ok := strings.Cut(r.Repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			errs = append(errs, fmt.Errorf("repo %q: repo must be owner/name", r.Repo))
			continue
		}
		key := strings.ToLower(r.Repo)
		if seen[key] {
			errs = append(errs, fmt.Errorf("repo %s: configured twice", r.Repo))
		}
		seen[key] = true
		if c.WatchFor(r.Repo) == nil {
			errs = append(errs, fmt.Errorf("repo %s: no [[watch]] covers it", r.Repo))
		}
		if c.PoolFor(r.Repo) != nil && r.worktreeKeys() {
			errs = append(errs, fmt.Errorf("repo %s: has a [[pool]]; only no_findings_event, blocking_event, prepare, ready, ready_timeout, keep_approvals and required_checks apply to it (setup, teardown, wt_hooks, copy_files, strip_env and env configure per-PR worktrees; use the pool's)", r.Repo))
		}
		for _, g := range r.RequiredChecks {
			// A malformed glob would match no check; Match reports it even for "".
			glob := strings.TrimPrefix(g, RequiredWorkflowPrefix)
			if strings.TrimSpace(glob) == "" {
				errs = append(errs, fmt.Errorf("repo %s: empty required_checks entry %q", r.Repo, g))
			} else if _, err := path.Match(glob, ""); err != nil {
				errs = append(errs, fmt.Errorf("repo %s: required_checks pattern %q: %w", r.Repo, g, err))
			}
		}
		for _, msg := range trackerProblems("repo "+r.Repo, r.Trackers) {
			errs = append(errs, errors.New(msg))
		}
		errs = append(errs, validateReadiness("repo "+r.Repo, r.Prepare, r.Ready, r.ReadyTimeout)...)
		errs = append(errs, validateVerdicts("repo "+r.Repo, r.NoFindingsEvent, r.BlockingEvent)...)
		for k := range r.Env {
			if !envKeyRe.MatchString(k) {
				errs = append(errs, fmt.Errorf("repo %s: env key %q is not a valid variable name", r.Repo, k))
			}
		}
		errs = append(errs, validateCopyFiles("repo "+r.Repo, r.CopyFiles)...)
		if slices.ContainsFunc(r.StripEnv, func(k string) bool { return strings.TrimSpace(k) == "" }) {
			errs = append(errs, fmt.Errorf("repo %s: empty strip_env entry", r.Repo))
		}
		for _, cmd := range append(slices.Clone(r.Setup), r.Teardown...) {
			if strings.TrimSpace(cmd) == "" {
				errs = append(errs, fmt.Errorf("repo %s: empty setup/teardown command", r.Repo))
			}
		}
	}
	return errs
}

// validateTrivialDeltas checks a skip_trivial_deltas list (prefix names
// where): every entry one of TrivialDeltaClasses.
func validateTrivialDeltas(prefix string, classes []string) []error {
	var errs []error
	for _, c := range classes {
		if !slices.Contains(TrivialDeltaClasses, c) {
			errs = append(errs, fmt.Errorf("%sskip_trivial_deltas: %q is not one of %s", prefix, c, strings.Join(TrivialDeltaClasses, ", ")))
		}
	}
	return errs
}

// validateReadiness checks the prepare, ready and ready_timeout keys of a
// [[pool]] or [[repo]] block (label names it): no empty or multi-line
// command, no negative timeout.
func validateReadiness(label string, prepare, ready []string, timeout Duration) []error {
	var errs []error
	for _, list := range []struct {
		key  string
		cmds []string
	}{{"prepare", prepare}, {"ready", ready}} {
		for _, cmd := range list.cmds {
			switch {
			case strings.TrimSpace(cmd) == "":
				errs = append(errs, fmt.Errorf("%s: empty %s command", label, list.key))
			case strings.ContainsAny(cmd, "\r\n"):
				errs = append(errs, fmt.Errorf("%s: %s command %q spans lines; use one entry per command", label, list.key, cmd))
			}
		}
	}
	if timeout.Duration < 0 {
		errs = append(errs, fmt.Errorf("%s: ready_timeout must not be negative", label))
	}
	return errs
}

// validateVerdicts checks the no_findings_event and blocking_event of an
// [[identity]] or [[repo]] block (label names it); "" means not set.
func validateVerdicts(label, noFindings, blocking string) []error {
	var errs []error
	switch noFindings {
	case "", "APPROVE", "COMMENT":
	default:
		errs = append(errs, fmt.Errorf("%s: no_findings_event must be APPROVE or COMMENT, got %q", label, noFindings))
	}
	switch blocking {
	case "", "REQUEST_CHANGES", "COMMENT":
	default:
		errs = append(errs, fmt.Errorf("%s: blocking_event must be REQUEST_CHANGES or COMMENT, got %q", label, blocking))
	}
	return errs
}

// validateCopyFiles checks the copy_files of a [[repo]] or [[pool]] block
// (label is "repo owner/name" or "pool owner/name"): every entry is
// non-empty and a path relative to the clone that stays inside it.
func validateCopyFiles(label string, files []string) []error {
	var errs []error
	for _, f := range files {
		if strings.TrimSpace(f) == "" {
			errs = append(errs, fmt.Errorf("%s: empty copy_files entry", label))
			continue
		}
		if clean := filepath.Clean(f); filepath.IsAbs(f) || clean == ".." || strings.HasPrefix(clean, "../") {
			errs = append(errs, fmt.Errorf("%s: copy_files entry %q must be relative to the clone", label, f))
		}
	}
	return errs
}

// validateQuietHours checks daemon.quiet_hours with the rules of
// internal/eligibility's parser, which imports this package and so cannot be
// called from here: empty is fine, else "HH:MM-HH:MM" with each clock in
// 00:00..23:59 and start != end.
func validateQuietHours(spec string) []error {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return nil
	}
	from, to, found := strings.Cut(trimmed, "-")
	if !found {
		return []error{fmt.Errorf("daemon.quiet_hours %q: want HH:MM-HH:MM", spec)}
	}
	start, err := parseClock(from)
	if err != nil {
		return []error{fmt.Errorf("daemon.quiet_hours %q: start: %w", spec, err)}
	}
	end, err := parseClock(to)
	if err != nil {
		return []error{fmt.Errorf("daemon.quiet_hours %q: end: %w", spec, err)}
	}
	if start == end {
		return []error{fmt.Errorf("daemon.quiet_hours %q: start and end are the same, so the window is empty", spec)}
	}
	return nil
}

// parseClock reads "H:MM" or "HH:MM" as minutes since midnight.
func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM: %w", strings.TrimSpace(s), err)
	}
	return t.Hour()*60 + t.Minute(), nil
}

var (
	roleNameRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	roleAliasRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,23}$`)
	kindNameRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// validatePipeline checks [pipeline], [kinds.*], [[role]] and the watches'
// role sets.
func (c *Config) validatePipeline() []error {
	var errs []error
	kinds := c.kinds()
	for _, name := range c.KindNames() {
		errs = append(errs, validateKind(name, kinds[name])...)
	}
	roles := c.roles()
	if len(roles) == 0 {
		errs = append(errs, errors.New("at least one [[role]] is required"))
	}
	ids := map[string]bool{}
	for _, id := range c.Identities {
		ids[id.Name] = true
	}
	seen := map[string]string{} // name or alias -> owning role
	outputs := map[string]string{}
	for _, r := range roles {
		for _, n := range append([]string{r.Name}, r.Aliases...) {
			if owner, dup := seen[n]; dup {
				errs = append(errs, fmt.Errorf("role %s: name or alias %q is already used by role %s", r.Name, n, owner))
				continue
			}
			seen[n] = r.Name
		}
		if owner, dup := outputs[r.ReportFile()]; dup {
			errs = append(errs, fmt.Errorf("role %s: output %q is already written by role %s", r.Name, r.ReportFile(), owner))
		}
		outputs[r.ReportFile()] = r.Name
		errs = append(errs, c.validateRole(r, kinds, ids)...)
	}
	errs = append(errs, validateAfter(roles)...)
	for _, w := range c.Watches {
		errs = append(errs, c.validateWatchRoles(w, roles)...)
	}
	return errs
}

func validateKind(name string, k Kind) []error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("kinds.%s: "+format, append([]any{name}, a...)...))
	}
	if !kindNameRe.MatchString(name) || name == KindShell {
		bad("name must match %s and not be %q", kindNameRe, KindShell)
	}
	switch k.Wrapper {
	case "", WrapperAuto, WrapperTrue, WrapperFalse:
	default:
		bad("wrapper must be auto, true or false, got %q", k.Wrapper)
	}
	switch k.SessionSource {
	case "", SessionHerdr, SessionNone:
	default:
		bad("session_source must be herdr or none, got %q", k.SessionSource)
	}
	switch k.OnPermissionPrompt {
	case "", PermissionDeny, PermissionWait:
	default:
		bad("on_permission_prompt must be deny or wait, got %q", k.OnPermissionPrompt)
	}
	switch k.OnHooksReview {
	case "", HooksTrustOwn, HooksDecline:
	default:
		bad("on_hooks_review must be trust_own or decline, got %q", k.OnHooksReview)
	}
	for _, g := range []struct {
		key, ph string
		args    []string
	}{{"resume", PlaceholderSession, k.Resume}, {"model", PlaceholderModel, k.Model},
		{"effort", PlaceholderEffort, k.Effort}, {"name", PlaceholderTitle, k.Name}} {
		if len(g.args) > 0 && !slices.ContainsFunc(g.args, func(s string) bool { return strings.Contains(s, g.ph) }) {
			bad("%s %q must contain %s", g.key, g.args, g.ph)
		}
	}
	if k.Rename != "" && !strings.Contains(k.Rename, PlaceholderTitle) {
		bad("rename %q must contain %s", k.Rename, PlaceholderTitle)
	}
	if k.SwitchModel != "" && !strings.Contains(k.SwitchModel, PlaceholderModel) {
		bad("switch_model %q must contain %s", k.SwitchModel, PlaceholderModel)
	}

	// Model names are typed into the pane: a space or control character
	// would split the command or submit it early.
	oneWord := func(s string) bool {
		return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	}
	seenModel := map[string]bool{}
	for _, mdl := range k.FallbackModels {
		if !oneWord(mdl) {
			bad("fallback_models: %q must be one word", mdl)
		}
		if seenModel[strings.ToLower(mdl)] {
			bad("fallback_models lists %q twice", mdl)
		}
		seenModel[strings.ToLower(mdl)] = true
	}
	for _, x := range []struct{ key, model string }{{"default_model", k.DefaultModel}, {"reset_model", k.ResetModel}} {
		if x.model != "" && !oneWord(x.model) {
			bad("%s %q must be one word", x.key, x.model)
		}
	}
	if k.DefaultModel != "" && len(k.Model) == 0 {
		bad("default_model %q needs model args (model = [..., %q]) to be passed", k.DefaultModel, PlaceholderModel)
	}
	if _, _, err := parseLoginOK(k.LoginOK); err != nil {
		bad("%v", err)
	}
	if k.LoginOK != "" && strings.TrimSpace(k.LoginCheck) == "" {
		bad("login_ok needs login_check")
	}
	if _, err := k.HealthPatterns.Compile(); err != nil {
		bad("%v", err)
	}
	for key := range k.Env {
		if !envKeyRe.MatchString(key) {
			bad("env key %q is not a valid variable name", key)
		}
	}
	return errs
}

func (c *Config) validateRole(r Role, kinds map[string]Kind, ids map[string]bool) []error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("role %s: "+format, append([]any{r.Name}, a...)...))
	}
	if !roleNameRe.MatchString(r.Name) {
		bad("name must match %s", roleNameRe)
	}
	for _, a := range r.Aliases {
		if !roleAliasRe.MatchString(a) {
			bad("alias %q must match %s", a, roleAliasRe)
		}
	}
	kind, agentKind := kinds[r.Kind]
	switch {
	case r.Kind == KindShell:
		if r.Mode != ModeShell {
			bad("kind shell needs mode shell, got %q", r.Mode)
		}
	case !agentKind:
		bad("kind %q is not declared (declared: %s, or shell)", r.Kind, strings.Join(c.KindNames(), ", "))
	case r.Mode != ModeSession:
		bad("kind %s needs mode session, got %q", r.Kind, r.Mode)
	}
	switch r.Runs {
	case RunsAlways, RunsFirst, RunsManual, RunsNever:
	default:
		bad("runs must be always, first, manual or never, got %q", r.Runs)
	}
	if r.RerunMinLines < 0 { // only runs = "first" reads it; elsewhere it is ignored
		bad("rerun_min_lines must be 0 or more, got %d", r.RerunMinLines)
	}
	switch r.Capture {
	case CaptureFile, CaptureGitDiff:
	case CaptureStdout:
		if !r.IsShell() {
			bad("capture stdout is for shell roles only")
		}
	default:
		bad("capture must be file, stdout or git-diff, got %q", r.Capture)
	}
	if r.Judge {
		if r.IsShell() {
			bad("a judge must be an agent session, not a shell command")
		}
		if r.Runs != RunsAlways {
			bad("a judge must have runs = always")
		}
		if r.Capture != CaptureFile {
			bad("a judge must have capture = file (it writes its result file)")
		}
		if len(r.After) > 0 {
			bad("a judge takes no after (it always runs last)")
		}
	} else if r.Skill != "" {
		bad("skill is for judges only")
	}
	if r.IsShell() {
		switch {
		case r.Command == "" && r.Prompt == "":
			bad("a shell role needs command (or prompt naming a full-line shell template)")
		case r.Command != "" && r.Prompt != "":
			bad("set command or prompt, not both")
		}
		if r.Tool != "" {
			if _, ok := kinds[r.Tool]; !ok {
				bad("tool %q is not a declared kind", r.Tool)
			}
		}
		if r.Model != "" {
			bad("model is for agent roles only")
		}
	} else {
		if r.Command != "" {
			bad("command is for shell roles only")
		}
		if r.Tool != "" {
			bad("tool is for shell roles only (an agent role uses its kind)")
		}
		if r.Model != "" && agentKind && len(kind.Model) == 0 {
			bad("kind %s has no model args ([kinds.%s] model), so model %q cannot be passed", r.Kind, r.Kind, r.Model)
		}
		if len(r.OKStatus) > 0 {
			bad("ok_status is for shell roles only")
		}
	}
	for _, status := range r.OKStatus {
		if status < 0 || status > 255 {
			bad("ok_status %d must be 0..255", status)
		}
	}
	if r.Identity != "" && !ids[r.Identity] {
		bad("unknown identity %q", r.Identity)
	}
	if out := r.ReportFile(); out == "." || out == ".." || strings.ContainsAny(out, `/\`) {
		bad("output %q must be a plain file name", out)
	}
	if r.Timeout.Duration <= 0 {
		bad("timeout must be positive")
	}
	for key := range r.Env {
		if !envKeyRe.MatchString(key) {
			bad("env key %q is not a valid variable name", key)
		}
	}
	parsed := map[string]bool{} // rereview often names the initial prompt's file
	for _, kind := range PromptKinds {
		name := r.PromptFile(kind)
		if name == "" {
			continue
		}
		p, err := c.ResolvePrompt(name)
		if err != nil {
			bad("%s prompt: %v", kind, err)
			continue
		}
		if parsed[name] {
			continue
		}
		parsed[name] = true
		if err := parseTemplate(name, p.Text); err != nil {
			bad("%s prompt %s: %v", kind, name, err)
		}
	}
	if r.IsShell() && r.Command != "" {
		if err := parseTemplate(r.Name+" command", r.Command); err != nil {
			bad("command: %v", err)
		}
	}
	return errs
}

// parseTemplate checks that text is a valid Go template the way
// internal/agents renders prompts and commands (no funcs, missingkey=error),
// without executing it.
func parseTemplate(name, text string) error {
	_, err := template.New(name).Option("missingkey=error").Parse(text)
	return err
}

// validateAfter checks that After names declared non-judge roles other than
// the role itself and that the graph is acyclic.
func validateAfter(roles []Role) []error {
	var errs []error
	byName := map[string]Role{}
	for _, r := range roles {
		byName[r.Name] = r
	}
	for _, r := range roles {
		for _, a := range r.After {
			dep, ok := byName[a]
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("role %s: after names unknown role %q", r.Name, a))
			case a == r.Name:
				errs = append(errs, fmt.Errorf("role %s: after names itself", r.Name))
			case dep.Judge:
				errs = append(errs, fmt.Errorf("role %s: after names judge %s (the judge always runs last)", r.Name, a))
			}
		}
	}
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var visit func(name string, path []string) bool
	visit = func(name string, path []string) bool {
		switch state[name] {
		case visiting:
			errs = append(errs, fmt.Errorf("roles: after forms a cycle: %s", strings.Join(append(path, name), " -> ")))
			return false
		case done:
			return true
		}
		state[name] = visiting
		for _, a := range byName[name].After {
			if _, ok := byName[a]; ok && a != name && !visit(a, append(path, name)) {
				return false
			}
		}
		state[name] = done
		return true
	}
	for _, r := range roles {
		if !visit(r.Name, nil) {
			break
		}
	}
	return errs
}

func (c *Config) validateWatchRoles(w Watch, roles []Role) []error {
	var errs []error
	seen := map[string]bool{}
	for _, s := range w.Roles {
		i := slices.IndexFunc(roles, func(r Role) bool { return r.Matches(s) })
		if i < 0 {
			errs = append(errs, fmt.Errorf("watch %s: unknown role %q", w.Owner, s))
			continue
		}
		if seen[roles[i].Name] {
			errs = append(errs, fmt.Errorf("watch %s: role %s listed twice", w.Owner, roles[i].Name))
		}
		seen[roles[i].Name] = true
	}
	var judges []string
	for _, r := range c.RolesFor(&w) {
		if r.Judge {
			judges = append(judges, r.Name)
		}
	}
	if len(judges) != 1 {
		errs = append(errs, fmt.Errorf("watch %s: needs exactly one judge among its roles, has %d (%s); set roles = [...]",
			w.Owner, len(judges), strings.Join(judges, ", ")))
	}
	return errs
}

// minKeep is the shortest retention daemon.keep_events and keep_requests
// accept: `magnum logs` and the board read a day of events, and a CLI may
// wait on a request for hours.
const minKeep = 24 * time.Hour

// validateKeep checks a retention: 0 keeps rows forever, anything else must
// be at least minKeep.
func validateKeep(key string, d Duration) []error {
	if d.Duration == 0 || d.Duration >= minKeep {
		return nil
	}
	return []error{fmt.Errorf("%s must be 0 (keep forever) or at least 1d, got %s", key, d.Duration)}
}

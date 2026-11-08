package config

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"

	"github.com/zhuravel/magnum/internal/paths"
)

// Factory is [factory]: the factory daemon that writes code from a Jira key
// or a prompt (internal/factory). The code knows a few generic stage types;
// everything specific to a repository or a team is configuration, as
// magnum's own roles and kinds are. config.defaults.toml ships the roles,
// the kinds of task and their stages, and names no repository and no staging
// environment (those are [[factory.repo]] blocks of the user's config).
//
// The user config merges into the built-in values key by key where that is
// safe: [factory.kinds.<name>] replaces that kind of task and keeps the
// others, a [[factory.role]] named like a role overrides the keys it sets
// (any other name is appended), [factory.local_review] and the three tables
// codex_fallback, kind_by_jira_type and kind_words merge entry by entry, and
// [[factory.repo]] blocks are appended. The other keys replace the value.
type Factory struct {
	// MaxActive is how many tasks may have live agents or a held copy at
	// once; a task over it waits queued.
	MaxActive int `toml:"max_active"`
	// MemoryGateSwap is the swap use, in percent of the swap file, at which
	// new tasks wait for memory (the same reading as `magnum doctor`);
	// 0 = no gate.
	MemoryGateSwap int `toml:"memory_gate_swap"`
	// HeavyStages is how many heavy stages run at once across tasks (a local
	// review round, local QA or a copy's server). At least 1.
	HeavyStages int `toml:"heavy_stages"`
	// HerdrSocket is the socket of the herdr session the factory's panes
	// live in; "" = the named session's own socket. Absolute after ~
	// expands.
	HerdrSocket string `toml:"herdr_socket"`
	// HerdrSession is the name of that herdr session (`herdr --session
	// <name>`): a separate server with its own windows, apart from the
	// panes of magnum's reviews.
	HerdrSession string `toml:"herdr_session"`
	// Browser is the headless Chrome the factory starts for a qa stage, an
	// absolute path after ~ expands; "" = the first of
	// DefaultFactoryBrowsers that exists.
	Browser string `toml:"browser"`
	// DiffPager is the pager of the screen's d, the diff of a ready card
	// (GIT_PAGER, which git prefers over core.pager), such as "hunk pager";
	// "" = git's own. The card's file order stays: it is git's diff.
	DiffPager string `toml:"diff_pager"`
	// Observe is how often the factory reads its panes; Poll how often its
	// main tick runs (the holds, your requests, the published tasks' PRs as
	// magnum's registry has them, then a step of each live task); a request
	// kicks a tick at once. The factory itself reads nothing from GitHub
	// after the publish.
	Observe Duration `toml:"observe"`
	Poll    Duration `toml:"poll"`
	// HumanCooldown is how long the factory leaves a pane alone after you
	// type into it.
	HumanCooldown Duration `toml:"human_cooldown"`

	// Sensitive are the path globs (MatchPath's: "**" matches whole
	// directories) of the files whose change the ready card's FLAGS line
	// names for a second look: by default Gemfile.lock, the workflows and
	// the config files.
	Sensitive []string `toml:"sensitive"`

	// LocalReview is magnum's review loop on the change before the PR.
	LocalReview FactoryLocalReview `toml:"local_review"`
	// Repos are the repositories the factory writes code in
	// ([[factory.repo]]), appended from every layer. A repository needs a
	// [[watch]] that covers it: the first local round runs the review roles
	// of that watch.
	Repos []FactoryRepo `toml:"repo"`
	// Roles are the factory's agents ([[factory.role]]): session roles that
	// each carry a why and a doing (Role.Why, Role.Doing). After Load they
	// hold the spend level of their kind as the review roles do: its notch
	// on the built-in roles' efforts and its [spend.<level>] table
	// (applySpend).
	Roles []Role `toml:"role"`
	// CodexFallback maps a factory role that runs on Codex to the factory
	// role of the Claude kind that takes its work after a Codex refusal. The
	// user's table merges entry by entry, and an empty value removes the
	// built-in entry. An entry whose key is a role that no longer runs on
	// Codex (a [[factory.role]] moved it to Claude) is not an error: it is
	// not used, so read it only for a role of the Codex kind (Warnings says
	// so).
	CodexFallback map[string]string `toml:"codex_fallback"`
	// Kinds are the kinds of task ([factory.kinds.<name>]) and their stages.
	Kinds map[string]FactoryKind `toml:"kinds"`
	// KindByJiraType maps a Jira issue type to a kind of task, KindWords a
	// kind of task to the words of a free prompt that suggest it.
	KindByJiraType map[string]string   `toml:"kind_by_jira_type"`
	KindWords      map[string][]string `toml:"kind_words"`
}

// FactoryLocalReview is [factory.local_review]. Round 1 runs the review
// roles of the repository's [[watch]]; later rounds run without LaterWithout.
type FactoryLocalReview struct {
	// Cycles is the most rounds the loop runs (1 to MaxFactoryCycles).
	Cycles int `toml:"cycles"`
	// LaterWithout names the review roles that run in round 1 only.
	LaterWithout []string `toml:"later_without"`
}

// FactoryRepo is a [[factory.repo]]: a repository the factory writes code
// in, with the copies of its checkout the tasks work in. The copies are a
// block of their own, never a second [[pool]] of the repository (Pool turns
// them into the Pool the slot code provisions).
type FactoryRepo struct {
	Repo string `toml:"repo"` // owner/name
	// Base is the branch tasks start from and the PR targets (default
	// master). The factory never merges into it.
	Base      string `toml:"base"`
	MainClone string `toml:"main_clone"`
	// Copies is the path of copy {n}; CopyName its name (default
	// factory{n}). Neither may render a name or path a [[pool]] renders.
	Copies   string `toml:"copies"`
	CopyName string `toml:"copy_name"`
	// MaxCopies is how many copies exist at most (default [factory]
	// max_active); MinFreeDiskGB the free disk a copy needs (default
	// [daemon] min_free_disk_gb).
	MaxCopies     int `toml:"max_copies"`
	MinFreeDiskGB int `toml:"min_free_disk_gb"`
	// Setup, Teardown, PostCheckout and ResetDB are the commands of a
	// copy's life, as a [[pool]] has them.
	Setup        []string `toml:"setup"`
	Teardown     []string `toml:"teardown"`
	PostCheckout []string `toml:"post_checkout"`
	ResetDB      []string `toml:"reset_db"`
	// Installs run Run when Lock differs from the one the copy last
	// installed.
	Installs []FactoryInstall `toml:"installs"`
	// Server is the command that starts a copy's local server.
	Server string `toml:"server"`
	// Env is the copy's environment: {copy} (or {slot}) renders the copy's
	// name, {n} its number and {n*16} its Redis offset, and REDIS_DB_OFFSET,
	// when it is named, is copy n's n*16 whatever its value (the factory's
	// copies). Databases its databases, named like a pool's
	// (DatabaseSuffix).
	Env       map[string]string `toml:"env"`
	Databases []string          `toml:"databases"`
	CopyFiles []string          `toml:"copy_files"`
	StripEnv  []string          `toml:"strip_env"`
	// Reviewer is the GitHub login the factory requests as the PR's
	// reviewer (milestone 2).
	Reviewer string          `toml:"reviewer"`
	Precheck FactoryPrecheck `toml:"precheck"`
	// ErrorSkill is the read-only error-tracker skill an investigator reads
	// the ticket's error through. GhostMarker is what a migration names the
	// environments with, where a person changes the table with gh-ost;
	// TableStats the file (relative to the checkout) that lists the size of
	// each table, and LargeTableRows the row count from which a table is
	// large (a change to it needs gh-ost or an online algorithm). Consoles
	// are the remote console commands that no factory command runs. None has
	// a default.
	ErrorSkill     string   `toml:"error_skill"`
	GhostMarker    string   `toml:"ghost_marker"`
	TableStats     string   `toml:"table_stats"`
	LargeTableRows int64    `toml:"large_table_rows"`
	Consoles       []string `toml:"consoles"`
	// PRBodySkill is the skill the judge writes a published PR's description
	// with, the team's own layout ("" = none: the factory's outline). It is
	// a skill's name, trimmed, one word; no default.
	PRBodySkill string `toml:"pr_body_skill"`
	// Staging are the staging environments, in the order they are listed.
	Staging []FactoryStaging `toml:"staging"`
	// QA is [factory.repo.qa], how a qa stage looks at the change's pages in
	// the copy's own server; nil = no QA for the repository.
	QA *FactoryQA `toml:"qa"`
}

// FactoryQA is [factory.repo.qa]: what a qa stage needs to walk the pages a
// change shows. The factory starts the copy's server (the repository's
// Server, on the PORT of its Env), waits until URL answers Ready, and has
// the fx-qa walker look at the pages through the factory's own headless
// browser, for at most Cap.
type FactoryQA struct {
	// When reads the changed files on stdin and prints the user-facing ones,
	// one per line, in the copy (required): none printed, no QA; a failing
	// command means "cannot tell", never "nothing user-facing". It has 30
	// seconds: a quick filter of paths, which boots nothing.
	When string `toml:"when"`
	// URL is the copy's server as a browser reaches it, with {port} as its
	// port (required): "http://app.example.localhost:{port}". Its host is on
	// this Mac (LocalHost).
	URL string `toml:"url"`
	// Ready is the path whose 2xx or 3xx answer says the server is up
	// (default DefaultQAReady).
	Ready string `toml:"ready"`
	// Skill is the repository's QA skill, one word, that the walker reads
	// for its sign-in, test data and page conventions ("" = none).
	Skill string `toml:"skill"`
	// SignIn is one line that says how the walker signs in locally ("" =
	// none): never a password, but the env var that holds one.
	SignIn string `toml:"sign_in"`
	// ServerStop runs in the copy before the server's process group is
	// killed (such as "overmind kill"); "" = none.
	ServerStop string `toml:"server_stop"`
	// Boot is how long the server may take to answer Ready (default
	// DefaultQABoot), Cap how long one run may take in all, the boot
	// included (default DefaultQACap, at most MaxQACap).
	Boot Duration `toml:"boot"`
	Cap  Duration `toml:"cap"`
	// MaxPages is how many pages the walker captures at most (default
	// DefaultQAMaxPages, at most MaxQAPages); Fixes how many writer turns
	// QA's defects get before the task waits on you (default DefaultQAFixes,
	// at most MaxQAFixes).
	MaxPages int `toml:"max_pages"`
	Fixes    int `toml:"fixes"`
}

// PageURL is the page at path of the copy whose server listens on port: URL
// with {port} rendered, then path, one slash between them whether path
// starts with one or not (so a path never runs into the port, where "@x"
// would make x the host).
func (q FactoryQA) PageURL(port int, path string) string {
	base := strings.ReplaceAll(q.URL, "{port}", strconv.Itoa(port))
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

// LocalHost reports whether host (a URL's hostname, without brackets or a
// port) names this Mac: localhost, a name under .localhost, 127.0.0.1 or
// ::1, case ignored.
func LocalHost(host string) bool {
	h := strings.ToLower(host)
	switch {
	case h == "localhost", h == "127.0.0.1", h == "::1":
		return true
	case strings.HasSuffix(h, ".localhost"):
		return len(h) > len(".localhost")
	}
	return false
}

// FactoryInstall is an `installs` entry of a repository: Run runs when the
// file Lock differs from the copy's last install.
type FactoryInstall struct {
	Lock string `toml:"lock"`
	Run  string `toml:"run"`
}

// FactoryPrecheck is [factory.repo.precheck]: what runs after the writer's
// last commit and after each fix of the local review. A command takes
// {files} (the changed files, shell-quoted) and {base}; a migrations
// command also {version} (the digits a migration's file name starts with)
// and {file}, and runs once per migration the branch adds.
type FactoryPrecheck struct {
	Lint       []string `toml:"lint"`
	Specs      string   `toml:"specs"`
	Jest       string   `toml:"jest"`
	Migrations []string `toml:"migrations"`
	// Cap stops a precheck that runs this long (default DefaultPrecheckCap).
	Cap Duration `toml:"cap"`
	// SkipSpecsForKinds names the kinds of task whose precheck skips the
	// specs (a gem upgrade).
	SkipSpecsForKinds []string `toml:"skip_specs_for_kinds"`
}

// FactoryStaging is a `staging` entry of a repository: an environment with
// a branch the factory merges into after your approval. QA names the skill
// that tests it ("" = no QA); After the environment whose QA must pass first.
// The first milestone reads none of it, but it validates.
type FactoryStaging struct {
	Name   string `toml:"name"`
	Branch string `toml:"branch"`
	QA     string `toml:"qa"`
	After  string `toml:"after"`
}

// FactoryKind is a kind of task: the stages it runs, in order, and what its
// coding stage's proof needs (Proof): FactoryProofRed (the default, ""), a
// spec of the change that fails on the base and passes at the head (a bug,
// a feature), or FactoryProofGreen, its specs green at the head with no run
// on the base (a chore: the existing specs stay green).
type FactoryKind struct {
	Stages []FactoryStage `toml:"stages"`
	Proof  string         `toml:"proof"`
}

// RedRun reports whether the kind's proof needs its spec red on the base.
func (k FactoryKind) RedRun() bool { return k.Proof != FactoryProofGreen }

// The proofs a kind may name (FactoryKind.Proof).
const (
	FactoryProofRed   = "red"
	FactoryProofGreen = "green"
)

// FactoryStage is one stage of a kind of task. Its Type picks the code that
// runs it and which of the other fields it takes: an agents stage launches
// Roles in parallel and ends when its End fact holds; a gate waits until its
// Gate holds; a review stage runs the local review loop (End "clean"); a
// decision waits for your Decision; an effect does its Effect (End "pr").
// When "wide" skips the stage unless the plan touches 3 or more files, the
// database or security. Prompt, for an agents stage, is the prompt file
// every role of the stage gets in place of its own role's (a stage that
// asks a role for another file than its usual one, such as the judge's PR
// body); empty, each role's own.
type FactoryStage struct {
	Name     string   `toml:"name"`
	Type     string   `toml:"type"`
	Roles    []string `toml:"roles"`
	End      string   `toml:"end"`
	Gate     string   `toml:"gate"`
	Decision string   `toml:"decision"`
	Effect   string   `toml:"effect"`
	When     string   `toml:"when"`
	Prompt   string   `toml:"prompt"`
}

// The stage types, end facts, gates, decisions, effects and conditions a
// FactoryStage may name.
const (
	FactoryStageAgents   = "agents"
	FactoryStageGate     = "gate"
	FactoryStageReview   = "review"
	FactoryStageDecision = "decision"
	FactoryStageEffect   = "effect"
	FactoryStageQA       = "qa"

	FactoryEndFindings = "findings"
	FactoryEndReport   = "report"
	FactoryEndCritic   = "critic"
	FactoryEndProof    = "proof"
	FactoryEndClean    = "clean"
	FactoryEndPRBody   = "pr_body"
	FactoryEndPR       = "pr"
	FactoryEndQA       = "qa"

	FactoryGateNoOpenQuestions = "no_open_questions"
	FactoryGateReproduced      = "reproduced"

	FactoryDecisionReadyPush = "ready_push"

	FactoryEffectPublish = "publish"

	FactoryWhenWide = "wide"
)

// FactoryStageTypes, FactoryEndFacts, FactoryGates, FactoryDecisions and
// FactoryEffects list what a stage may name, in the order the docs give.
var (
	FactoryStageTypes = []string{FactoryStageAgents, FactoryStageGate, FactoryStageReview, FactoryStageDecision, FactoryStageEffect, FactoryStageQA}
	FactoryEndFacts   = []string{FactoryEndFindings, FactoryEndReport, FactoryEndCritic, FactoryEndProof, FactoryEndClean, FactoryEndPRBody, FactoryEndPR, FactoryEndQA}
	FactoryGates      = []string{FactoryGateNoOpenQuestions, FactoryGateReproduced}
	FactoryDecisions  = []string{FactoryDecisionReadyPush}
	FactoryEffects    = []string{FactoryEffectPublish}
)

// Factory defaults that a repository without the key takes (expand), and
// the most rounds of the local review.
const (
	DefaultPrecheckCap   = 10 * time.Minute
	DefaultFactoryBase   = "master"
	DefaultFactoryCopies = "factory{n}"
	MaxFactoryCycles     = 10
)

// The defaults and the limits of a repository's [factory.repo.qa]
// (FactoryQA).
const (
	DefaultQABoot     = 5 * time.Minute
	DefaultQACap      = 50 * time.Minute
	DefaultQAMaxPages = 12
	DefaultQAFixes    = 2
	DefaultQAReady    = "/"
	MaxQAPages        = 30
	MaxQAFixes        = 5
	MaxQACap          = 3 * time.Hour
)

// DefaultFactoryBrowsers are the browsers the factory starts for QA when
// [factory] browser names none: the first that exists.
var DefaultFactoryBrowsers = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
}

// DefaultFactory returns the built-in [factory] values, the ones
// config.defaults.toml writes out (TestFactoryGoDefaultsEqualTheEmbeddedFile
// holds the two together). The roles are not normalized: Normalize does.
func DefaultFactory() Factory {
	return Factory{
		MaxActive:      3,
		MemoryGateSwap: 80,
		HeavyStages:    1,
		HerdrSession:   "factory",
		Observe:        Duration{10 * time.Second},
		Poll:           Duration{45 * time.Second},
		HumanCooldown:  Duration{15 * time.Minute},
		Sensitive:      []string{"Gemfile.lock", ".github/workflows/**", "config/**"},
		LocalReview:    FactoryLocalReview{Cycles: 3, LaterWithout: []string{RoleClaudeSimplify}},
		Roles:          DefaultFactoryRoles(),
		CodexFallback:  map[string]string{"fx-cause": "fx-cause-claude", "fx-critic": "fx-critic-claude"},
		Kinds:          DefaultFactoryKinds(),
		KindByJiraType: map[string]string{"Bug": "bug", "Story": "feature", "Task": "chore"},
		KindWords: map[string][]string{
			"bug":     {"bug", "fix", "broken", "crash", "error", "regression", "fails"},
			"feature": {"add", "feature", "implement", "support", "new"},
			"chore":   {"upgrade", "bump", "dependency", "gem", "rename", "cleanup", "refactor"},
		},
	}
}

// DefaultFactoryRoles are the factory's built-in agents. The prompts are the
// factory-*.md files of package prompts.
func DefaultFactoryRoles() []Role {
	const (
		investigate = 40 * time.Minute
		short       = 30 * time.Minute
	)
	return []Role{
		{Name: "fx-repro", Kind: KindClaude, Effort: "high", Prompt: "factory-repro.md", Timeout: Duration{investigate},
			Why: "show the bug with a failing spec", Doing: "reproducing it"},
		{Name: "fx-cause", Kind: KindCodex, Effort: "high", Prompt: "factory-cause.md", Timeout: Duration{investigate},
			Why: "find the cause on its own, apart from the others", Doing: "finding the cause"},
		{Name: "fx-cause-claude", Kind: KindClaude, Effort: "high", Prompt: "factory-cause.md", Timeout: Duration{investigate},
			Why: "find the cause on its own, apart from the others", Doing: "finding the cause"},
		{Name: "fx-history", Kind: KindClaude, Effort: "medium", Prompt: "factory-history.md", Timeout: Duration{short},
			Why: "how the code changed, and why", Doing: "reading the history"},
		// fx-points's one recorded run took 19 minutes at high: an hour leaves an xhigh run room.
		{Name: "fx-points", Kind: KindClaude, Effort: "high", Prompt: "factory-points.md", Timeout: Duration{time.Hour},
			Why: "test each point of the ticket against the code", Doing: "checking the ticket's points"},
		{Name: "fx-scope", Kind: KindClaude, Effort: "high", Prompt: "factory-scope.md", Timeout: Duration{investigate},
			Why: "find every place the chore touches", Doing: "finding what it touches"},
		{Name: "fx-critic", Kind: KindCodex, Effort: "xhigh", Prompt: "factory-critic.md", Timeout: Duration{short},
			Why: "read the plan as a critic and ask what it missed", Doing: "criticizing the plan"},
		{Name: "fx-critic-claude", Kind: KindClaude, Effort: "high", Prompt: "factory-critic.md", Timeout: Duration{short},
			Why: "read the plan as a critic and ask what it missed", Doing: "criticizing the plan"},
		{Name: "fx-judge", Kind: KindClaude, Model: "opus", Effort: "xhigh", Prompt: "factory-judge.md", Timeout: Duration{45 * time.Minute},
			Why: "merge the findings into one report and the questions", Doing: "weighing the findings"},
		{Name: "fx-writer", Kind: KindClaude, Model: "opus", Effort: "high", Prompt: "factory-writer.md", Timeout: Duration{90 * time.Minute},
			Why: "write the change, test first", Doing: "writing the change"},
		{Name: "fx-qa", Kind: KindClaude, Effort: "high", Prompt: "factory-qa.md", Timeout: Duration{35 * time.Minute},
			Why: "look at the changed pages in a browser, in the states the ticket names", Doing: "walking the pages"},
		// The liaison talks with you about a task; no stage names it. It asks you itself (AskUserQuestion), so
		// magnum leaves its dialogs to you.
		{Name: "fx-liaison", Kind: KindClaude, Effort: "high", Prompt: "factory-liaison.md", Timeout: Duration{24 * time.Hour},
			OnPermissionPrompt: PermissionWait,
			Why:                "talk with you about the task: ask its questions, explain, act on your word", Doing: "talking with you"},
	}
}

// notchFixedFactoryRoles are the built-in factory roles whose effort no spend
// level's notch moves (factoryNotchRoles): a history summary gains little
// from xhigh. A level's table still sets them, as it sets any role.
var notchFixedFactoryRoles = []string{"fx-history"}

// factoryNotchRoles are the built-in factory roles the spend level's notch
// reaches (applySpend's built-ins): all but notchFixedFactoryRoles.
func factoryNotchRoles() []Role {
	return slices.DeleteFunc(DefaultFactoryRoles(), func(r Role) bool { return slices.Contains(notchFixedFactoryRoles, r.Name) })
}

// DefaultFactoryKinds are the built-in kinds of task. They differ in what
// investigates (the bug's reproducer, cause and history, the feature's
// points, the chore's scope) and in the bug's gate on a reproduction; the
// rest is the same: a judge, a critic for a wide plan, one batch of
// questions, the writer, the local review, visual QA of the change's pages
// (a repository without [factory.repo.qa] skips it), the PR body, your
// decision, and the publish that only the decision leads to.
func DefaultFactoryKinds() map[string]FactoryKind {
	kind := func(investigators []string, gates ...FactoryStage) FactoryKind {
		stages := []FactoryStage{
			{Name: "investigate", Type: FactoryStageAgents, Roles: investigators, End: FactoryEndFindings},
			{Name: "judge", Type: FactoryStageAgents, Roles: []string{"fx-judge"}, End: FactoryEndReport},
			{Name: "critic", Type: FactoryStageAgents, Roles: []string{"fx-critic"}, End: FactoryEndCritic, When: FactoryWhenWide},
			{Name: "questions", Type: FactoryStageGate, Gate: FactoryGateNoOpenQuestions},
		}
		stages = append(stages, gates...)
		return FactoryKind{Stages: append(stages,
			FactoryStage{Name: "coding", Type: FactoryStageAgents, Roles: []string{"fx-writer"}, End: FactoryEndProof},
			FactoryStage{Name: "local_review", Type: FactoryStageReview, End: FactoryEndClean},
			FactoryStage{Name: "qa", Type: FactoryStageQA, Roles: []string{"fx-qa"}, End: FactoryEndQA},
			FactoryStage{Name: "pr_body", Type: FactoryStageAgents, Roles: []string{"fx-judge"}, End: FactoryEndPRBody, Prompt: "factory-pr-body.md"},
			FactoryStage{Name: "ready", Type: FactoryStageDecision, Decision: FactoryDecisionReadyPush},
			FactoryStage{Name: "publish", Type: FactoryStageEffect, Effect: FactoryEffectPublish, End: FactoryEndPR},
		)}
	}
	chore := kind([]string{"fx-scope"})
	chore.Proof = FactoryProofGreen // its specs stay green: no red run on the base
	return map[string]FactoryKind{
		"bug":     kind([]string{"fx-repro", "fx-cause", "fx-history"}, FactoryStage{Name: "reproduction", Type: FactoryStageGate, Gate: FactoryGateReproduced}),
		"feature": kind([]string{"fx-points"}),
		"chore":   chore,
	}
}

// FactoryRepo returns the [[factory.repo]] for owner/name (case ignored).
func (c *Config) FactoryRepo(full string) (FactoryRepo, bool) {
	for _, r := range c.Factory.Repos {
		if strings.EqualFold(r.Repo, full) {
			return r, true
		}
	}
	return FactoryRepo{}, false
}

// FactoryRole returns the factory role named name (an exact name: a factory
// role takes no alias).
func (c *Config) FactoryRole(name string) (Role, bool) {
	for _, r := range c.Factory.Roles {
		if r.Name == name {
			return r, true
		}
	}
	return Role{}, false
}

// Pool is the repository's copies as the Pool the slot code provisions:
// slots named CopyName at Copies, at most MaxCopies of them, none kept
// ready (Min 0), the commands and databases the repository sets. The
// slices and the map are copies.
func (r FactoryRepo) Pool() Pool {
	return Pool{
		Repo:            r.Repo,
		MainClone:       r.MainClone,
		SlotName:        r.CopyName,
		SlotPath:        r.Copies,
		Base:            r.Base,
		Max:             r.MaxCopies,
		IdleRemoveAfter: Duration{DefaultIdleRemoveAfter},
		MinFreeDiskGB:   r.MinFreeDiskGB,
		CopyFiles:       slices.Clone(r.CopyFiles),
		StripEnv:        slices.Clone(r.StripEnv),
		Setup:           slices.Clone(r.Setup),
		Teardown:        slices.Clone(r.Teardown),
		ResetDB:         slices.Clone(r.ResetDB),
		PostCheckout:    slices.Clone(r.PostCheckout),
		Databases:       slices.Clone(r.Databases),
		Env:             maps.Clone(r.Env),
	}
}

// mergeFactory finishes overlayFactory. overlaySection has replaced every key
// the layer defines (src, with metadata md and rawRoles its [[factory.role]]
// tables as written) in c.Factory, whole; prev is c.Factory as it was. The
// repositories are appended, the roles override by name key by key, the kinds
// of task by name, the three tables entry by entry, and [factory.local_review]
// key by key. See Factory. The keys a role block of a layer that is not the
// embedded defaults (builtin) sets are the operator's (factorySet): the
// spend level's notch leaves them alone (buildPipeline).
func (c *Config) mergeFactory(md toml.MetaData, prev, src Factory, rawRoles []map[string]any, builtin bool) {
	f := &c.Factory
	f.Repos = append(slices.Clone(prev.Repos), src.Repos...)
	roles := prev.Roles
	for i, r := range src.Roles {
		if !builtin && i < len(rawRoles) {
			if c.factorySet == nil {
				c.factorySet = map[string]map[string]bool{}
			}
			markKeys(c.factorySet, r.Name, rawRoles[i])
		}
		if j := slices.IndexFunc(roles, func(x Role) bool { return x.Name == r.Name }); j >= 0 {
			roles[j] = mergeRole(roles[j], r, rawRoles, i)
		} else {
			roles = append(roles, mergeRole(Role{}, r, rawRoles, i))
		}
	}
	f.Roles = roles
	f.Kinds = mergedMap(prev.Kinds, src.Kinds)
	f.CodexFallback = mergedMap(prev.CodexFallback, src.CodexFallback)
	maps.DeleteFunc(f.CodexFallback, func(_, fallback string) bool { return fallback == "" })
	f.KindByJiraType = mergedMap(prev.KindByJiraType, src.KindByJiraType)
	f.KindWords = mergedMap(prev.KindWords, src.KindWords)
	f.LocalReview = prev.LocalReview
	mergeDefined(reflect.ValueOf(&f.LocalReview).Elem(), reflect.ValueOf(src.LocalReview), nil, func(p []string) bool {
		return md.IsDefined(append([]string{"factory", "local_review"}, p...)...)
	})
}

// mergedMap is base with the entries of over set on top, in a new map.
func mergedMap[K comparable, V any](base, over map[K]V) map[K]V {
	out := maps.Clone(base)
	if out == nil {
		out = map[K]V{}
	}
	maps.Copy(out, over)
	return out
}

// readFactoryRoles reads the [[factory.role]] tables of a layer as written
// (their key sets are what a role overrides): from data, or from file when
// data is nil. nil when the layer writes none.
func readFactoryRoles(file string, data []byte, md toml.MetaData) ([]map[string]any, error) {
	if !md.IsDefined("factory", "role") {
		return nil, nil
	}
	var raw struct {
		Factory struct {
			Role []map[string]any `toml:"role"`
		} `toml:"factory"`
	}
	var err error
	if data != nil {
		_, err = toml.Decode(string(data), &raw)
	} else {
		_, err = toml.DecodeFile(file, &raw)
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", file, err)
	}
	return raw.Factory.Role, nil
}

// expandFactory resolves ~ in the factory's paths and fills the defaults of
// each repository (Load only: it runs after the layers merged).
func (c *Config) expandFactory() {
	f := &c.Factory
	f.HerdrSocket = paths.Expand(f.HerdrSocket)
	f.Browser = paths.Expand(f.Browser)
	for i := range f.Repos {
		r := &f.Repos[i]
		r.MainClone = paths.Expand(r.MainClone)
		r.PRBodySkill = strings.TrimSpace(r.PRBodySkill)
		if r.Base == "" {
			r.Base = DefaultFactoryBase
		}
		if r.CopyName == "" {
			r.CopyName = DefaultFactoryCopies
		}
		if r.MaxCopies == 0 {
			r.MaxCopies = f.MaxActive
		}
		if r.MinFreeDiskGB == 0 {
			r.MinFreeDiskGB = c.Daemon.MinFreeDiskGB
		}
		if r.Precheck.Cap.Duration == 0 {
			r.Precheck.Cap = Duration{DefaultPrecheckCap}
		}
		if r.QA != nil {
			q := *r.QA // the layer's block stays as it was decoded
			q.expand()
			r.QA = &q
		}
	}
}

// expand fills the defaults of the keys a [factory.repo.qa] leaves out and
// trims the skill.
func (q *FactoryQA) expand() {
	q.Skill = strings.TrimSpace(q.Skill)
	if q.Ready == "" {
		q.Ready = DefaultQAReady
	}
	if q.Boot.Duration == 0 {
		q.Boot = Duration{DefaultQABoot}
	}
	if q.Cap.Duration == 0 {
		q.Cap = Duration{DefaultQACap}
	}
	if q.MaxPages == 0 {
		q.MaxPages = DefaultQAMaxPages
	}
	if q.Fixes == 0 {
		q.Fixes = DefaultQAFixes
	}
}

var (
	factoryNameRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	factoryStageRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	herdrSessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// validateFactory checks [factory] always (a base file or a test may run
// the factory whatever the repositories say): the limits, the intervals, the
// local review, each role, the codex fallbacks, each kind of task and its
// stages, the pickers of a kind, and each repository with its copies, its
// precheck and its staging environments.
func (c *Config) validateFactory() []error {
	f := c.Factory
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if f.MaxActive < 1 {
		bad("factory.max_active must be at least 1, got %d", f.MaxActive)
	}
	if f.MemoryGateSwap < 0 || f.MemoryGateSwap > 100 {
		bad("factory.memory_gate_swap must be between 0 (no gate) and 100 percent, got %d", f.MemoryGateSwap)
	}
	if f.HeavyStages < 1 {
		bad("factory.heavy_stages must be at least 1, got %d", f.HeavyStages)
	}
	if f.HerdrSocket != "" && !filepath.IsAbs(f.HerdrSocket) {
		bad("factory.herdr_socket must be absolute after expansion (or empty for the session's own socket): %s", f.HerdrSocket)
	}
	if f.Browser != "" && !filepath.IsAbs(f.Browser) {
		bad("factory.browser must be absolute after expansion (or empty for the first of %s that exists): %s",
			strings.Join(DefaultFactoryBrowsers, ", "), f.Browser)
	}
	if !herdrSessionRe.MatchString(f.HerdrSession) {
		bad("factory.herdr_session %q must be a session name (letters, digits, . _ -)", f.HerdrSession)
	}
	for _, d := range []struct {
		key string
		v   time.Duration
	}{{"observe", f.Observe.Duration}, {"poll", f.Poll.Duration}} {
		if d.v <= 0 {
			bad("factory.%s must be positive, got %s", d.key, d.v)
		}
	}
	if d := f.HumanCooldown.Duration; d < 0 {
		bad("factory.human_cooldown must not be negative, got %s", d)
	}
	for _, g := range f.Sensitive {
		if err := ValidatePathGlob(g); err != nil {
			bad("factory.sensitive entry %q: %v", g, err)
		}
	}
	errs = append(errs, f.LocalReview.validate()...)
	errs = append(errs, c.validateFactoryRoles()...)
	errs = append(errs, c.validateFactoryKinds()...)
	errs = append(errs, c.validateFactoryPickers()...)
	errs = append(errs, c.validateFactoryRepos()...)
	return errs
}

func (l FactoryLocalReview) validate() []error {
	var errs []error
	if l.Cycles < 1 || l.Cycles > MaxFactoryCycles {
		errs = append(errs, fmt.Errorf("factory.local_review.cycles must be between 1 and %d, got %d", MaxFactoryCycles, l.Cycles))
	}
	seen := map[string]bool{}
	for _, name := range l.LaterWithout {
		switch {
		case !roleAliasRe.MatchString(name):
			errs = append(errs, fmt.Errorf("factory.local_review.later_without entry %q must be a review role's name", name))
		case seen[name]:
			errs = append(errs, fmt.Errorf("factory.local_review.later_without lists %q twice", name))
		}
		seen[name] = true
	}
	return errs
}

// validateFactoryRoles checks each [[factory.role]] with the checks of a
// [[role]] (validateRole), worded for the section, plus what makes it a
// factory agent: an agent session that is no judge, fix or whiteboard role,
// takes no alias and no identity, has a why and a doing of one line each,
// and a name no other factory role and no review role has.
func (c *Config) validateFactoryRoles() []error {
	var errs []error
	reviewNames := map[string]string{}
	for _, r := range c.roles() {
		for _, n := range append([]string{r.Name}, r.Aliases...) {
			reviewNames[n] = r.Name
		}
	}
	seen := map[string]bool{}
	for _, r := range c.Factory.Roles {
		bad := func(format string, a ...any) {
			errs = append(errs, fmt.Errorf("factory.role %s: "+format, append([]any{r.Name}, a...)...))
		}
		switch owner, review := reviewNames[r.Name]; {
		case seen[r.Name]:
			bad("name is already used by another factory role")
		case review:
			bad("name is already used by the review role %s", owner)
		}
		seen[r.Name] = true
		switch {
		case r.Judge:
			bad("a factory role cannot be a judge (judge = true is the review pipeline's)")
		case r.Fix:
			bad("a factory role cannot be a fix role")
		case r.Whiteboard:
			bad("a factory role cannot be a whiteboard role")
		case r.IsShell():
			bad("must be an agent session, not a shell command")
		}
		if len(r.Aliases) > 0 {
			bad("aliases are not used by factory roles (stages and codex_fallback name a role by its name)")
		}
		for _, w := range []struct{ key, v string }{{"why", r.Why}, {"doing", r.Doing}} {
			if strings.TrimSpace(w.v) == "" || strings.ContainsAny(w.v, "\r\n") {
				bad("%s must be one line of text (the factory's screen names the agent by it), got %q", w.key, w.v)
			}
		}
		if r.Identity != "" {
			bad("identity is not used by factory roles (agents act as you)")
			r.Identity = ""
		}
		for _, err := range c.validateRole(r, c.kinds(), nil) {
			errs = append(errs, errors.New("factory."+err.Error()))
		}
	}
	return errs
}

// validateFactoryKinds checks each kind of task: a name, at least one
// stage, and every stage against its type.
func (c *Config) validateFactoryKinds() []error {
	var errs []error
	roles := map[string]Role{}
	for _, r := range c.Factory.Roles {
		roles[r.Name] = r
	}
	for _, name := range slices.Sorted(maps.Keys(c.Factory.Kinds)) {
		k := c.Factory.Kinds[name]
		where := "factory kind " + name
		if !factoryNameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("%s: the name must match %s", where, factoryNameRe))
		}
		if len(k.Stages) == 0 {
			errs = append(errs, fmt.Errorf("%s: needs at least one stage", where))
		}
		if k.Proof != "" && k.Proof != FactoryProofRed && k.Proof != FactoryProofGreen {
			errs = append(errs, fmt.Errorf("%s: proof %q must be %s (a spec red on the base, green at the head) or %s (green at the head only)",
				where, k.Proof, FactoryProofRed, FactoryProofGreen))
		}
		seen := map[string]bool{}
		decided := false   // a ready_push decision stage came before
		reviewed := false  // a review stage came before
		published := false // a publish effect stage came before
		qa := ""           // the kind's qa stage, once one came
		for _, st := range k.Stages {
			bad := func(format string, a ...any) {
				errs = append(errs, fmt.Errorf("%s: stage %q: "+format, append([]any{where, st.Name}, a...)...))
			}
			switch {
			case !factoryStageRe.MatchString(st.Name):
				bad("the name must match %s", factoryStageRe)
			case seen[st.Name]:
				bad("is listed twice")
			}
			seen[st.Name] = true
			errs = append(errs, c.validateStage(where, st, roles)...)
			if st.Type == FactoryStageDecision && st.Decision == FactoryDecisionReadyPush {
				decided = true
			}
			if st.Type == FactoryStageEffect && st.Effect == FactoryEffectPublish && !decided {
				bad("the %s effect must come after a %s decision stage (nothing leaves the Mac without your y)", FactoryEffectPublish, FactoryDecisionReadyPush)
			}
			if st.Type == FactoryStageQA {
				switch {
				case qa != "":
					bad("a kind has at most one qa stage, and %q is one already", qa)
				case !reviewed:
					bad("a qa stage must come after a review stage (QA looks at a change the review passed)")
				case decided || published:
					bad("a qa stage must come before the %s decision and the %s effect (the ready card needs QA at its head)",
						FactoryDecisionReadyPush, FactoryEffectPublish)
				}
				qa = st.Name
			}
			if st.Type == FactoryStageReview {
				reviewed = true
			}
			if st.Type == FactoryStageEffect && st.Effect == FactoryEffectPublish {
				published = true
			}
		}
	}
	return errs
}

// validateStage checks one stage against its type: the fields its type
// needs are set and valid (an agents stage's prompt resolves and parses),
// and the fields of another type are empty.
func (c *Config) validateStage(where string, st FactoryStage, roles map[string]Role) []error {
	var errs []error
	bad := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: stage %q: "+format, append([]any{where, st.Name}, a...)...))
	}
	oneOf := func(key, v string, list []string) {
		if !slices.Contains(list, v) {
			bad("%s %q must be one of %s", key, v, strings.Join(list, ", "))
		}
	}
	if !slices.Contains(FactoryStageTypes, st.Type) {
		bad("type %q must be one of %s", st.Type, strings.Join(FactoryStageTypes, ", "))
		return errs
	}
	switch {
	case st.Type == FactoryStageQA && st.When != "":
		bad("when %q is not for a qa stage (the repository's qa.when says whether a change shows pages)", st.When)
	case st.When != "" && st.When != FactoryWhenWide:
		bad("when %q must be empty or %q", st.When, FactoryWhenWide)
	}
	switch {
	case st.Type == FactoryStageQA && st.End != FactoryEndQA:
		bad("end %q must be %s (the walker's verdict and the factory's own checks of the pages)", st.End, FactoryEndQA)
	case st.Type != FactoryStageQA && st.End == FactoryEndQA:
		bad("end %q is for qa stages only", st.End)
	case st.End != "":
		oneOf("end", st.End, FactoryEndFacts)
	}
	if st.Type != FactoryStageAgents && st.Type != FactoryStageQA && len(st.Roles) > 0 {
		bad("roles are for agents and qa stages only")
	}
	if st.Type != FactoryStageAgents && st.Prompt != "" {
		bad("prompt is for agents stages only")
	}
	if st.Type != FactoryStageGate && st.Gate != "" {
		bad("gate is for gate stages only")
	}
	if st.Type != FactoryStageDecision && st.Decision != "" {
		bad("decision is for decision stages only")
	}
	if st.Type != FactoryStageEffect && st.Effect != "" {
		bad("effect is for effect stages only")
	}
	if (st.Type == FactoryStageGate || st.Type == FactoryStageDecision) && st.End != "" {
		bad("end is for agents, review, effect and qa stages")
	}
	if st.Type == FactoryStageAgents || st.Type == FactoryStageQA {
		switch {
		case len(st.Roles) == 0:
			bad("needs roles")
		case st.Type == FactoryStageQA && len(st.Roles) > 1:
			bad("a qa stage names exactly one role (one walker drives the one browser tab and writes the one verdict), got %d", len(st.Roles))
		}
		for _, r := range st.Roles {
			if _, ok := roles[r]; !ok {
				bad("names unknown role %q (declare it as a [[factory.role]])", r)
			}
		}
	}
	switch st.Type {
	case FactoryStageAgents:
		if st.End == "" {
			bad("needs an end fact (one of %s)", strings.Join(FactoryEndFacts, ", "))
		}
		if st.Prompt != "" {
			if p, err := c.ResolvePrompt(st.Prompt); err != nil {
				bad("prompt: %v", err)
			} else if _, err := ParseTemplate(p.Name, p.Text, p.Shared); err != nil {
				bad("prompt %s: %v", p.Name, err)
			}
		}
	case FactoryStageGate:
		oneOf("gate", st.Gate, FactoryGates)
	case FactoryStageDecision:
		oneOf("decision", st.Decision, FactoryDecisions)
	case FactoryStageEffect:
		oneOf("effect", st.Effect, FactoryEffects)
	}
	return errs
}

// validateFactoryPickers checks the tables that pick a kind of task or a
// fallback: each must name a kind or a role that exists.
func (c *Config) validateFactoryPickers() []error {
	f := c.Factory
	var errs []error
	known := strings.Join(slices.Sorted(maps.Keys(f.Kinds)), ", ")
	for _, jt := range slices.Sorted(maps.Keys(f.KindByJiraType)) {
		if strings.TrimSpace(jt) == "" {
			errs = append(errs, errors.New("factory.kind_by_jira_type has an empty Jira issue type"))
		}
		if _, ok := f.Kinds[f.KindByJiraType[jt]]; !ok {
			errs = append(errs, fmt.Errorf("factory.kind_by_jira_type %q names the kind %q, which is not one of %s", jt, f.KindByJiraType[jt], known))
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(f.KindWords)) {
		if _, ok := f.Kinds[kind]; !ok {
			errs = append(errs, fmt.Errorf("factory.kind_words names the kind %q, which is not one of %s", kind, known))
		}
		for _, w := range f.KindWords[kind] {
			if strings.TrimSpace(w) == "" {
				errs = append(errs, fmt.Errorf("factory.kind_words.%s has a blank word", kind))
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(f.CodexFallback)) {
		fallback := f.CodexFallback[key]
		// A key that is a role of another kind is only a warning
		// (factoryWarnings): a [[factory.role]] may move a role to Claude.
		if _, ok := c.FactoryRole(key); !ok {
			errs = append(errs, fmt.Errorf("factory.codex_fallback key %q names no [[factory.role]]", key))
		}
		switch r, ok := c.FactoryRole(fallback); {
		case !ok:
			errs = append(errs, fmt.Errorf("factory.codex_fallback %s: the fallback %q names no [[factory.role]]", key, fallback))
		case r.AgentKind() != KindClaude:
			errs = append(errs, fmt.Errorf("factory.codex_fallback %s: the fallback %s must be a claude role, not %s", key, fallback, r.AgentKind()))
		}
	}
	return errs
}

// validateFactoryRepos checks each [[factory.repo]]. The copies' names and
// paths must differ from every [[pool]] slot's and from the other factory
// repositories' (slot names and paths are unique across pools).
func (c *Config) validateFactoryRepos() []error {
	var errs []error
	seen := map[string]bool{}
	type owner struct{ who, key, template string }
	names, dirs := map[string]owner{}, map[string]owner{}
	for _, p := range c.Pools {
		if !strings.Contains(p.SlotName, "{n}") || !strings.Contains(p.SlotPath, "{n}") {
			continue // reported by the pool's own checks
		}
		for n := 1; n <= min(p.Max, maxCheckedSlots); n++ {
			names[p.Slot(n)] = owner{"pool " + p.Repo, "slot_name", p.SlotName}
			dirs[filepath.Clean(p.Path(n))] = owner{"pool " + p.Repo, "slot_path", p.SlotPath}
		}
	}
	for i := range c.Factory.Repos {
		r := c.Factory.Repos[i]
		where := "factory repo " + r.Repo
		bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(where+": "+format, a...)) }
		switch key := strings.ToLower(r.Repo); {
		case !strings.Contains(r.Repo, "/"):
			errs = append(errs, fmt.Errorf("factory repo %q needs owner/name", r.Repo))
		case seen[key]:
			bad("configured twice")
		default:
			seen[key] = true
		}
		if r.MainClone == "" {
			bad("needs main_clone")
		}
		if strings.ContainsFunc(r.Base, unicode.IsSpace) {
			bad("base %q must be a branch name", r.Base)
		}
		hasN := strings.Contains(r.Copies, "{n}") && strings.Contains(r.CopyName, "{n}")
		if !strings.Contains(r.Copies, "{n}") {
			bad("copies %q must contain {n}", r.Copies)
		}
		if !strings.Contains(r.CopyName, "{n}") {
			bad("copy_name %q must contain {n}", r.CopyName)
		}
		if r.MaxCopies < 1 {
			bad("max_copies must be at least 1, got %d", r.MaxCopies)
		}
		if r.MinFreeDiskGB < 0 {
			bad("min_free_disk_gb must not be negative, got %d", r.MinFreeDiskGB)
		}
		if hasN && r.MaxCopies >= 1 {
			pool, last := r.Pool(), min(r.MaxCopies, maxCheckedSlots)
			for _, k := range []struct {
				key, template string
				seen          map[string]owner
				render        func(n int) string
			}{
				{"copy_name", r.CopyName, names, pool.Slot},
				{"copies", r.Copies, dirs, func(n int) string { return filepath.Clean(pool.Path(n)) }},
			} {
				for n := 1; n <= last; n++ {
					if o, taken := k.seen[k.render(n)]; taken {
						bad("%s %q renders %q for copy %d, which %s also renders from its %s %q; "+
							"slot names and paths are unique, so give the copies their own", k.key, k.template, k.render(n), n, o.who, o.key, o.template)
						break
					}
				}
				for n := 1; n <= last; n++ {
					if _, taken := k.seen[k.render(n)]; !taken {
						k.seen[k.render(n)] = owner{where, k.key, k.template}
					}
				}
			}
		}
		errs = append(errs, validateCopyFiles(where, r.CopyFiles)...)
		for _, d := range r.Databases {
			if base, ok := strings.CutSuffix(d, DatabaseSuffix); !ok || base == "" || strings.ContainsAny(base, "{}") {
				bad("database template %q must be <name>%s with no other placeholder (magnum lists, and drops, a copy's databases by that suffix)", d, DatabaseSuffix)
			}
		}
		for key := range r.Env {
			if !envKeyRe.MatchString(key) {
				bad("env key %q is not a valid variable name", key)
			}
		}
		for _, l := range []struct {
			key  string
			list []string
		}{{"setup", r.Setup}, {"teardown", r.Teardown}, {"post_checkout", r.PostCheckout}, {"reset_db", r.ResetDB}, {"strip_env", r.StripEnv},
			{"consoles", r.Consoles}, {"precheck.lint", r.Precheck.Lint}, {"precheck.migrations", r.Precheck.Migrations}} {
			if slices.ContainsFunc(l.list, func(s string) bool { return strings.TrimSpace(s) == "" }) {
				bad("%s has a blank entry", l.key)
			}
		}
		for _, in := range r.Installs {
			if strings.TrimSpace(in.Lock) == "" || strings.TrimSpace(in.Run) == "" {
				bad("an installs entry needs lock and run, got %+v", in)
			}
		}
		if r.Reviewer != "" {
			errs = append(errs, validateLogins(where+": ", "reviewer", []string{r.Reviewer})...)
			if strings.HasPrefix(r.Reviewer, "@") {
				bad("reviewer %q must be a GitHub login without the @", r.Reviewer)
			}
		}
		if strings.ContainsFunc(r.ErrorSkill, unicode.IsSpace) {
			bad("error_skill %q must be the skill's name, one word", r.ErrorSkill)
		}
		if skill := strings.TrimSpace(r.PRBodySkill); strings.ContainsFunc(skill, unicode.IsSpace) {
			bad("pr_body_skill %q must be the skill's name, one word", skill)
		}
		if r.LargeTableRows < 0 {
			bad("large_table_rows must not be negative, got %d", r.LargeTableRows)
		}
		if d := r.Precheck.Cap.Duration; d <= 0 {
			bad("precheck.cap must be positive, got %s", d)
		}
		for _, kind := range r.Precheck.SkipSpecsForKinds {
			if _, ok := c.Factory.Kinds[kind]; !ok {
				bad("precheck.skip_specs_for_kinds names %q, which is not a kind of task (%s)", kind, strings.Join(slices.Sorted(maps.Keys(c.Factory.Kinds)), ", "))
			}
		}
		errs = append(errs, validateFactoryStaging(where, r)...)
		if r.QA != nil {
			errs = append(errs, validateFactoryQA(where, r)...)
		}
		if c.WatchFor(r.Repo) == nil {
			bad("no [[watch]] covers it (the local review runs the review roles of its watch)")
		}
	}
	return errs
}

// validateFactoryQA checks a repository's [factory.repo.qa]: a when command;
// a url on this Mac whose port is {port}, with no login, query or fragment;
// a ready path; a skill of one word and a sign-in of one line; a boot within
// the cap and the cap, the pages and the fix turns within their limits; and
// the server and the copy's PORT that QA starts and walks.
func validateFactoryQA(where string, r FactoryRepo) []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(where+": "+format, a...)) }
	q := r.QA
	if strings.TrimSpace(q.When) == "" {
		bad("qa.when must be the command that prints the user-facing files of the changed ones it reads on stdin")
	}
	switch u, err := url.Parse(strings.ReplaceAll(q.URL, "{port}", "1")); {
	case err != nil:
		bad("qa.url %q does not parse: %v", q.URL, err)
	case u.Scheme != "http" && u.Scheme != "https":
		bad("qa.url %q must be http or https", q.URL)
	case !LocalHost(u.Hostname()):
		bad("qa.url %q must be on this Mac: localhost, a name under .localhost, 127.0.0.1 or ::1", q.URL)
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		bad("qa.url %q must carry no login, query or fragment", q.URL)
	case u.Port() != "1" || !strings.Contains(q.URL, "{port}"):
		bad("qa.url %q must name the copy's port as {port} (http://app.example.localhost:{port})", q.URL)
	}
	if !strings.HasPrefix(q.Ready, "/") {
		bad("qa.ready %q must be a path that starts with /", q.Ready)
	}
	if strings.ContainsFunc(q.Skill, unicode.IsSpace) {
		bad("qa.skill %q must be the skill's name, one word", q.Skill)
	}
	if strings.ContainsAny(q.SignIn, "\r\n") {
		bad("qa.sign_in must be one line")
	}
	switch boot, limit := q.Boot.Duration, q.Cap.Duration; {
	case boot <= 0:
		bad("qa.boot must be positive, got %s", boot)
	case boot > limit:
		bad("qa.boot %s must be at most qa.cap %s (the boot is part of the run)", boot, limit)
	}
	if limit := q.Cap.Duration; limit > MaxQACap {
		bad("qa.cap must be at most %s, got %s", MaxQACap, limit)
	}
	if q.MaxPages < 1 || q.MaxPages > MaxQAPages {
		bad("qa.max_pages must be between 1 and %d, got %d", MaxQAPages, q.MaxPages)
	}
	if q.Fixes < 1 || q.Fixes > MaxQAFixes {
		bad("qa.fixes must be between 1 and %d, got %d", MaxQAFixes, q.Fixes)
	}
	if strings.TrimSpace(r.Server) == "" {
		bad("qa needs server, the command that starts the copy's server")
	}
	if strings.TrimSpace(r.Env["PORT"]) == "" {
		bad(`qa needs the copy's port: env must name PORT (such as PORT = "31{n}0")`)
	} else if err := validateCopyPorts(r); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", where, err))
	}
	return errs
}

// validateCopyPorts checks that the repository's env PORT renders, for each
// of its copies, a port from 1024 to 65535 that no other copy has: QA walks
// the copy's server on it, and a port that does not parse is no port at all
// (the copy's Port is then 0). It renders as the factory renders a copy's
// env: {copy} or {slot} the copy's name, {n} its number, {n*16} n × 16.
func validateCopyPorts(r FactoryRepo) error {
	tmpl := r.Env["PORT"]
	pool := r.Pool()
	seen := map[int]int{}
	for n := 1; n <= min(r.MaxCopies, maxCheckedSlots); n++ {
		v := strings.NewReplacer("{copy}", pool.Slot(n), "{slot}", pool.Slot(n), "{n*16}", strconv.Itoa(n*16), "{n}", strconv.Itoa(n)).Replace(tmpl)
		port, err := strconv.Atoi(v)
		switch {
		case err != nil || port < 1024 || port > 65535:
			return fmt.Errorf("env PORT %q renders %q for copy %d: qa needs a port from 1024 to 65535", tmpl, v, n)
		case seen[port] != 0:
			return fmt.Errorf("env PORT %q renders %d for copies %d and %d: each copy's server needs a port of its own (use {n})", tmpl, port, seen[port], n)
		}
		seen[port] = n
	}
	return nil
}

// validateFactoryStaging checks a repository's staging environments: a
// unique name, a branch that is not the base, and an `after` that names
// another environment of the repository, with no cycle.
func validateFactoryStaging(where string, r FactoryRepo) []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(where+": "+format, a...)) }
	names := map[string]FactoryStaging{}
	for _, s := range r.Staging {
		switch {
		case strings.TrimSpace(s.Name) == "":
			bad("staging needs a name")
			continue
		case names[s.Name].Name != "":
			bad("staging %q is declared twice", s.Name)
		}
		names[s.Name] = s
		switch {
		case strings.TrimSpace(s.Branch) == "":
			bad("staging %q needs a branch", s.Name)
		case strings.ContainsFunc(s.Branch, unicode.IsSpace):
			bad("staging %q: branch %q must be a branch name", s.Name, s.Branch)
		case s.Branch == r.Base:
			bad("staging %q: branch %q is the base branch, which the factory never merges into", s.Name, s.Branch)
		}
	}
	for _, s := range r.Staging {
		switch _, ok := names[s.After]; {
		case s.After == "":
		case s.After == s.Name:
			bad("staging %q: after names itself", s.Name)
		case !ok:
			bad("staging %q: after names unknown staging %q", s.Name, s.After)
		}
	}
	state := map[string]int{} // 1 = on the path, 2 = done
	var visit func(name string, path []string) bool
	visit = func(name string, path []string) bool {
		switch state[name] {
		case 1:
			bad("staging after forms a cycle: %s", strings.Join(append(path, name), " -> "))
			return false
		case 2:
			return true
		}
		state[name] = 1
		if next := names[name].After; next != "" && next != name {
			if _, ok := names[next]; ok && !visit(next, append(path, name)) {
				return false
			}
		}
		state[name] = 2
		return true
	}
	for _, s := range r.Staging {
		if _, ok := names[s.Name]; ok && !visit(s.Name, nil) {
			break
		}
	}
	return errs
}

// factoryWarnings lists what is valid but probably not what you meant: a
// Codex factory role with no fallback (after a Codex refusal its task waits
// for you), a codex_fallback entry for a role that is not on Codex (the
// entry is not used), and, once a repository is configured, a later_without
// role no [[role]] has.
func (c *Config) factoryWarnings() []string {
	var out []string
	for _, r := range c.Factory.Roles {
		if r.AgentKind() == KindCodex && c.Factory.CodexFallback[r.Name] == "" {
			out = append(out, fmt.Sprintf("factory role %s runs on Codex and [factory] codex_fallback names no Claude role for it: after a Codex refusal its task waits for you", r.Name))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(c.Factory.CodexFallback)) {
		if r, ok := c.FactoryRole(key); ok && r.AgentKind() != KindCodex {
			out = append(out, fmt.Sprintf("factory.codex_fallback key %s is a %s role: its fallback is not used", key, r.AgentKind()))
		}
	}
	if len(c.Factory.Repos) > 0 {
		for _, name := range c.Factory.LocalReview.LaterWithout {
			if !slices.ContainsFunc(c.roles(), func(r Role) bool { return r.Matches(name) }) {
				out = append(out, fmt.Sprintf("factory.local_review.later_without names %q, which is no [[role]]: the later rounds run every review role", name))
			}
		}
	}
	return out
}

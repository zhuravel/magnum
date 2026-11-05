package factory

// The repository of a start, and the trackers of a repository that are not
// Jira (the operator's request of 2026-11-05). With two or more
// [[factory.repo]], a start that names none takes the one its text tells
// (inferRepo): a GitHub link of one, else a key of a tracker of exactly one.
// A key of a tracker that is not Jira (a Linear issue) is never read from
// Jira: at the start of the text it is the task's key, and the task starts
// from your words (startOther); in the words it is never found.

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/textx"
)

// The whys of a start's repository, in the words of the new task form's
// REPO row; repoFromTracker is the third.
const (
	repoFromLink = "from the link"
	repoYourPick = "your pick"
)

// repoFromTracker is the why of the repository a key's tracker tells: "from
// the EX tracker" for the prefix EX-.
func repoFromTracker(prefix string) string {
	return "from the " + strings.TrimSuffix(prefix, "-") + " tracker"
}

// whichRepo is a start that names no repository when there are several, and
// whose text tells none of them.
type whichRepo struct{ names []string }

func (w whichRepo) Error() string {
	return fmt.Sprintf("which repository? name one of %s (--repo)", strings.Join(w.names, ", "))
}

// inferRepo is the [[factory.repo]] a start's text tells, and why (the REPO
// row's words): with one repository, that one (why ""); else the repository
// of the text's first GitHub link that is one
// ("https://github.com/<owner>/<name>/..."), else that of its first key whose
// prefix tells one (repoPrefixes). ok is false when the text tells none of
// several: you pick one.
func inferRepo(cfg *config.Config, text string) (repo config.FactoryRepo, why string, ok bool) {
	repos := cfg.Factory.Repos
	if len(repos) == 1 {
		return repos[0], "", true
	}
	for _, raw := range textLink.FindAllString(text, -1) {
		u, err := url.Parse(strings.TrimRight(raw, ".,;:!?)]}"))
		if err != nil || !isGitHub(u.Hostname()) {
			continue
		}
		if owner, rest, _ := strings.Cut(strings.Trim(u.Path, "/"), "/"); owner != "" && rest != "" {
			name, _, _ := strings.Cut(rest, "/")
			if r, ok := cfg.FactoryRepo(owner + "/" + name); ok {
				return r, repoFromLink, true
			}
		}
	}
	tells, trackers := repoPrefixes(cfg)
	for _, key := range textKeys(text, trackers) {
		for prefix, i := range tells {
			if strings.HasPrefix(key, prefix) {
				return repos[i], repoFromTracker(prefix), true
			}
		}
	}
	return config.FactoryRepo{}, "", false
}

// repoPrefixes are the key prefixes that tell a factory repository (the
// index of the one each tells), and every tracker the factory repositories
// name. A prefix tells repository R when it is a tracker of R's own blocks
// (ownTrackers) and of no other repository's own blocks, and when no other
// factory repository sends it to another tracker (config.TrackersFor: one
// repository's PR- may lead to Linear while [board]'s leads to Jira). A
// [board] tracker that R's own blocks repeat is the same tracker, so it
// still tells R.
func repoPrefixes(cfg *config.Config) (map[string]int, []config.Tracker) {
	repos := cfg.Factory.Repos
	owners := map[string][]int{} // a prefix: the repositories whose own tracker it is
	resolved := make([]map[string]string, len(repos))
	var all []config.Tracker
	for i, r := range repos {
		for _, t := range ownTrackers(cfg, r.Repo) {
			if !slices.Contains(owners[t.Prefix], i) {
				owners[t.Prefix] = append(owners[t.Prefix], i)
			}
		}
		resolved[i] = map[string]string{}
		for _, t := range cfg.TrackersFor(r.Repo) {
			resolved[i][t.Prefix] = t.URL
			all = append(all, t)
		}
	}
	elsewhere := func(prefix string, i int) bool { // another repository sends prefix to another tracker
		for j := range repos {
			if url, ok := resolved[j][prefix]; j != i && ok && url != resolved[i][prefix] {
				return true
			}
		}
		return false
	}
	tells := map[string]int{}
	for prefix, is := range owners {
		if len(is) == 1 && !elsewhere(prefix, is[0]) { // the own tracker of several repositories tells none of them
			tells[prefix] = is[0]
		}
	}
	return tells, all
}

// textKeys are the keys of text in its order, as foundKeys reads them: a
// link of one of trackers is its key (trackerLinkKey), any other link is no
// key, and a bare key counts when nothing that makes a word touches it
// ("zhuravel-EX-12-fix" holds none).
func textKeys(text string, trackers []config.Tracker) []string {
	type hit struct {
		at  int
		key string
	}
	var hits []hit
	bare := []byte(text) // the text with its links blanked out
	for _, loc := range textLink.FindAllStringIndex(text, -1) {
		if key := trackerLinkKey(strings.TrimRight(text[loc[0]:loc[1]], ".,;:!?)]}"), trackers); key != "" {
			hits = append(hits, hit{loc[0], key})
		}
		for i := loc[0]; i < loc[1]; i++ {
			bare[i] = ' '
		}
	}
	for _, loc := range anyKey.FindAllIndex(bare, -1) {
		before, _ := utf8.DecodeLastRune(bare[:loc[0]])
		after, _ := utf8.DecodeRune(bare[loc[1]:])
		if !touchesKey(before) && !touchesKey(after) {
			hits = append(hits, hit{loc[0], string(bare[loc[0]:loc[1]])})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return a.at - b.at })
	keys := make([]string, len(hits))
	for i, h := range hits {
		keys[i] = h.key
	}
	return keys
}

// trackerLinkKey is the key of link when it is a link of one of trackers:
// its template's link, the query and the fragment left out, and anything
// after a slash (a Linear link's title slug) allowed; "" when it is none.
func trackerLinkKey(link string, trackers []config.Tracker) string {
	bare, _, _ := strings.Cut(link, "?")
	bare, _, _ = strings.Cut(bare, "#")
	for _, t := range trackers {
		if key, at := config.Issue(bare, []config.Tracker{t}); key != "" && (bare == at || strings.HasPrefix(bare, at+"/")) {
			return key
		}
	}
	return ""
}

// ownTrackers are the trackers of repo's own blocks: its [[repo]]'s and its
// covering [[watch]]'s. [board]'s are every repository's, so they tell none.
func ownTrackers(cfg *config.Config, repo string) []config.Tracker {
	var templates []string
	if r := cfg.RepoFor(repo); r != nil {
		templates = append(templates, r.Trackers...)
	}
	if w := cfg.WatchFor(repo); w != nil {
		templates = append(templates, w.Trackers...)
	}
	var out []config.Tracker
	for _, tmpl := range templates {
		if t, err := config.ParseTracker(tmpl); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// isJira reports whether tracker t is Jira's: its link is an atlassian.net
// site's /browse/ link, or it is on the [jira] site (config.Jira.Names).
func isJira(j config.Jira, t config.Tracker) bool {
	if j.Names(t.URL) {
		return true
	}
	u, err := url.Parse(t.URL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return (host == "atlassian.net" || strings.HasSuffix(host, ".atlassian.net")) && strings.HasPrefix(u.Path, "/browse/")
}

// otherTrackers are the trackers whose keys a task of repo never reads from
// Jira: repo's own (config.TrackersFor) that are not Jira's, and those of
// the other factory repositories that are not Jira's for a prefix repo has
// no tracker of. A key is read from Jira only when repo's tracker for it is
// Jira's, or when no factory repository names its prefix at all.
func (e *Engine) otherTrackers(repo string) []config.Tracker {
	own := e.cfg.TrackersFor(repo)
	var out []config.Tracker
	add := func(t config.Tracker) {
		if !isJira(e.cfg.Jira, t) && !slices.ContainsFunc(out, func(o config.Tracker) bool { return o.Prefix == t.Prefix }) {
			out = append(out, t)
		}
	}
	for _, t := range own {
		add(t)
	}
	for _, r := range e.cfg.Factory.Repos {
		for _, t := range e.cfg.TrackersFor(r.Repo) {
			if !slices.ContainsFunc(own, func(o config.Tracker) bool { return o.Prefix == t.Prefix }) {
				add(t)
			}
		}
	}
	return out
}

// otherTracker is the tracker of key when a task of repo never reads it from
// Jira (otherTrackers).
func (e *Engine) otherTracker(repo, key string) (config.Tracker, bool) {
	for _, t := range e.otherTrackers(repo) {
		if strings.HasPrefix(key, t.Prefix) {
			return t, true
		}
	}
	return config.Tracker{}, false
}

// trackerOf reports whether key is a key of one of trackers.
func trackerOf(key string, trackers []config.Tracker) bool {
	return slices.ContainsFunc(trackers, func(t config.Tracker) bool { return strings.HasPrefix(key, t.Prefix) })
}

// trackerName names a tracker in the words of a refusal and a prompt: its
// link's host ("linear.app").
func trackerName(t config.Tracker) string {
	if u, err := url.Parse(t.URL); err == nil && u.Hostname() != "" {
		return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	return t.Prefix + " tracker"
}

// jiraRepo reports whether repo's tasks may be Jira's: it has a Jira tracker,
// or no tracker at all (the [jira] site alone tells its links). A free prompt
// of a repository whose trackers are all not Jira's asks no question which
// Jira issue it is.
func (e *Engine) jiraRepo(repo string) bool {
	ts := e.cfg.TrackersFor(repo)
	return len(ts) == 0 || slices.ContainsFunc(ts, func(t config.Tracker) bool { return isJira(e.cfg.Jira, t) })
}

// readStart reads a start's text in repo (ParseStart). A text that starts
// with a key whose tracker a task of repo never reads from Jira
// (otherTrackers), bare or as a link of that tracker (a Linear issue's link,
// its title after the key or not), is that key's task: other is that
// tracker, ok true, and in holds the key and the words after it. The keys of
// such trackers in the words are never found.
func (e *Engine) readStart(text, repo string) (in StartInput, other config.Tracker, ok bool) {
	others := e.otherTrackers(repo)
	in = ParseStart(text, e.trackers(repo))
	word, words := splitStart(strings.TrimSpace(text))
	key := trackerLinkKey(word, others)
	if key == "" && in.Key != "" && trackerOf(in.Key, others) {
		key, words = in.Key, in.Guidance
	}
	for _, t := range others {
		if key != "" && strings.HasPrefix(key, t.Prefix) {
			return StartInput{Key: key, Guidance: words}, t, true
		}
	}
	in.Found = slices.DeleteFunc(in.Found, func(k string) bool { return trackerOf(k, others) })
	return in, config.Tracker{}, false
}

// otherAlone is the refusal of a key of tracker t, which is not Jira's, with
// no words after it: the factory reads no other tracker, so the task would
// have nothing to start from. It says why and what to do.
func otherAlone(key string, t config.Tracker) error {
	return fmt.Errorf("the factory reads no %s issue: add what to do after %s", trackerName(t), key)
}

// otherTitle is the title of a task of a key of another tracker: the key,
// then the first line of your words.
func otherTitle(in StartInput) string {
	return textx.Clip(strings.TrimSpace(in.Key+" "+textx.FirstLine(in.Guidance)), maxTitle)
}

// startOther starts the task of key in.Key of a tracker that is not Jira
// (readStart), from your words: the whole text is its prompt.md, the key is
// its own (one key, one task) and the first word of its title and branch, and
// no question asks which Jira issue it is. Nothing is read from Jira.
func (e *Engine) startOther(ctx context.Context, p StartPayload, repo config.FactoryRepo, login string, in StartInput,
	text string) (registry.Task, error) {
	kind, why := p.Kind, "you chose it"
	if kind == "" {
		kind, why = PickKind(e.cfg.Factory, nil, in.Guidance)
	}
	branch, err := e.branchFor(ctx, 0, repo.Repo, login, in.Key, in.Guidance)
	if err != nil {
		return registry.Task{}, err
	}
	state, who, what, err := e.startState(ctx)
	if err != nil {
		return registry.Task{}, err
	}
	t, err := e.reg.CreateTask(ctx, registry.Task{RequestID: p.request, JiraKey: in.Key, Prompt: text, Kind: kind, Repo: repo.Repo,
		Title: otherTitle(in), State: state, Branch: branch, WaitsWho: who, WaitsWhat: what})
	if err != nil {
		return e.heldBy(ctx, in.Key, err)
	}
	e.recordStart(ctx, t, p, StartInput{Prompt: text})
	if err := e.writeData(ctx, t, "ticket", "prompt.md", []byte(text+"\n")); err != nil {
		return t, err
	}
	e.event(ctx, t.ID, "intake", registry.PhaseOK, fmt.Sprintf("%s from your words: %s, whose tracker is not Jira, %s (%s), on %s",
		startVerb(t), in.Key, kind, why, branch))
	e.noteOverlap(ctx, t)
	return t, nil
}

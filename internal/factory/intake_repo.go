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
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

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
// row's words): the repository of its first GitHub link that is one
// ("https://github.com/<owner>/<name>/..."), else that of its first key whose
// prefix is a tracker of exactly one of them (ownTrackers), else the only one
// (why ""). ok is false when the text tells none of several: you pick one.
func inferRepo(cfg *config.Config, text string) (repo config.FactoryRepo, why string, ok bool) {
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
	owners := map[string][]int{} // a tracker's prefix: the repositories whose tracker it is
	for i, r := range cfg.Factory.Repos {
		for _, t := range ownTrackers(cfg, r.Repo) {
			if !slices.Contains(owners[t.Prefix], i) {
				owners[t.Prefix] = append(owners[t.Prefix], i)
			}
		}
	}
	at, best, prefix := -1, -1, ""
	for _, p := range slices.Sorted(maps.Keys(owners)) {
		if len(owners[p]) != 1 {
			continue // the tracker of several repositories tells none of them
		}
		if loc := keyOf(p).FindStringIndex(text); loc != nil && (at < 0 || loc[0] < at) {
			at, best, prefix = loc[0], owners[p][0], p
		}
	}
	switch {
	case best >= 0:
		return cfg.Factory.Repos[best], repoFromTracker(prefix), true
	case len(cfg.Factory.Repos) == 1:
		return cfg.Factory.Repos[0], "", true
	}
	return config.FactoryRepo{}, "", false
}

// keyOf matches a whole key of prefix, as a tracker finds its keys: "EX-12",
// not "XEX-12" or "EX-12a".
func keyOf(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(prefix) + `[0-9]+\b`)
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

// otherTrackers are repo's trackers (config.TrackersFor) that are not Jira's:
// their keys are never read from Jira.
func (e *Engine) otherTrackers(repo string) []config.Tracker {
	return slices.DeleteFunc(e.cfg.TrackersFor(repo), func(t config.Tracker) bool { return isJira(e.cfg.Jira, t) })
}

// trackerOf reports whether key is a key of one of trackers.
func trackerOf(key string, trackers []config.Tracker) bool {
	return slices.ContainsFunc(trackers, func(t config.Tracker) bool { return strings.HasPrefix(key, t.Prefix) })
}

// readStart reads a start's text in repo (ParseStart). A text that starts
// with a key of a tracker of the repository that is not Jira, bare or as a
// link of that tracker (a Linear issue's link, its title after the key or
// not), is that key's task: other is true, and in holds the key and the words
// after it. The keys of such trackers in the words are never found.
func (e *Engine) readStart(text, repo string) (in StartInput, other bool) {
	others := e.otherTrackers(repo)
	in = ParseStart(text, e.trackers(repo))
	word, words := splitStart(strings.TrimSpace(text))
	bare, _, _ := strings.Cut(word, "?")
	bare, _, _ = strings.Cut(bare, "#")
	for _, t := range others {
		if key, link := config.Issue(bare, []config.Tracker{t}); key != "" && (bare == link || strings.HasPrefix(bare, link+"/")) {
			return StartInput{Key: key, Guidance: words}, true
		}
	}
	if in.Key != "" && trackerOf(in.Key, others) {
		return StartInput{Key: in.Key, Guidance: in.Guidance}, true
	}
	in.Found = slices.DeleteFunc(in.Found, func(k string) bool { return trackerOf(k, others) })
	return in, false
}

// otherAlone is the refusal of a key of a tracker that is not Jira with no
// words after it: the factory reads no other tracker, so the task would have
// nothing to start from.
func otherAlone(key string) error {
	return fmt.Errorf("%s is not a Jira issue, and the factory reads no other tracker: say the task in your words after the key", key)
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

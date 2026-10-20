package relay

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Event is the routing envelope of one delivery, what the journal keeps and the listing answers: ids and
// routing only, never a title, a body, a comment, a patch, a user name or a URL.
type Event struct {
	// Seq is the event's place in the journal (its cursor is "<epoch>:<seq>").
	Seq int64 `json:"seq"`
	// DeliveryID is the delivery's X-GitHub-Delivery GUID, which a redelivery keeps.
	DeliveryID string `json:"delivery_id"`
	// Event is the X-GitHub-Event name and Action the payload's action ("" when it has none).
	Event  string `json:"event"`
	Action string `json:"action"`
	// InstallationID and RepositoryID are 0 when the payload has none.
	InstallationID int64 `json:"installation_id"`
	RepositoryID   int64 `json:"repository_id"`
	// Repository is "owner/name", "" when the payload has none.
	Repository string `json:"repository"`
	// PRNumbers are the pull requests the event names, ascending: the pull request of a pull_request*
	// event, the issue of an issue_comment on a pull request, a check's pull_requests (empty for a fork's,
	// whose HeadSHA is then the key), and none for a status.
	PRNumbers []int `json:"pr_numbers"`
	// HeadSHA is the pull request's head, the check's or the status's commit: 40 hex or "".
	HeadSHA string `json:"head_sha"`
	// BaseRef is a pull_request* event's base branch, else "".
	BaseRef string `json:"base_ref"`
	// Check is set only for check_run, check_suite and status.
	Check *Check `json:"check,omitempty"`
	// ReceivedAt is when the relay stored the event (UTC, whole seconds).
	ReceivedAt time.Time `json:"received_at"`
}

// Check is a CI event's name and conclusion: a check run's name and conclusion, a check suite's app name
// and conclusion, a status's context and state. Names are data, at most MaxCheckName runes; a conclusion is
// "" until there is one.
type Check struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
}

const (
	// MaxCheckName is the longest check name kept, in runes.
	MaxCheckName = 120
	// maxPRNumbers bounds the pull requests one event names.
	maxPRNumbers = 100
	maxBaseRef   = 255
)

// ErrPayload is a delivery whose payload is not a JSON document.
var ErrPayload = errors.New("payload is not JSON")

var (
	eventRe      = regexp.MustCompile(`^[a-z_]{1,64}$`)
	deliveryRe   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	repoRe       = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)
	shaRe        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	conclusionRe = regexp.MustCompile(`^[a-z_]{1,32}$`)
)

type prRef struct {
	Number int64 `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type prNumber struct {
	Number int64 `json:"number"`
}

// payload is the routing part of every event's payload; decoding into it drops every other field.
type payload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *prRef `json:"pull_request"`
	Issue       *struct {
		Number      int64           `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	CheckRun *struct {
		Name         string     `json:"name"`
		Conclusion   string     `json:"conclusion"`
		HeadSHA      string     `json:"head_sha"`
		PullRequests []prNumber `json:"pull_requests"`
	} `json:"check_run"`
	CheckSuite *struct {
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"head_sha"`
		App        struct {
			Name string `json:"name"`
		} `json:"app"`
		PullRequests []prNumber `json:"pull_requests"`
	} `json:"check_suite"`
	// A status event's commit, context and state.
	SHA     string `json:"sha"`
	Context string `json:"context"`
	State   string `json:"state"`
}

// Envelope reads the routing fields of a delivery's payload for its event name. A field of an unexpected
// shape, or a value that is not well formed (an id below 0, a SHA that is not 40 hex, a repository that is
// not owner/name), is left empty rather than refusing the delivery; a payload that is not JSON is ErrPayload.
// Seq, DeliveryID and ReceivedAt are the caller's.
func Envelope(event string, body []byte) (Event, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		// A value of another type than expected leaves its field empty, and the rest is decoded.
		if _, ok := errors.AsType[*json.UnmarshalTypeError](err); !ok {
			return Event{}, ErrPayload
		}
	}
	ev := Event{
		Event:          event,
		Action:         matched(eventRe, p.Action),
		InstallationID: max(p.Installation.ID, 0),
		RepositoryID:   max(p.Repository.ID, 0),
		Repository:     matched(repoRe, p.Repository.FullName),
	}
	var prs []int64
	switch event {
	case "pull_request", "pull_request_review", "pull_request_review_comment", "pull_request_review_thread":
		if pr := p.PullRequest; pr != nil {
			prs = []int64{pr.Number}
			ev.HeadSHA = matched(shaRe, pr.Head.SHA)
			ev.BaseRef = refName(pr.Base.Ref)
		}
	case "issue_comment":
		if is := p.Issue; is != nil && len(is.PullRequest) > 0 && string(is.PullRequest) != "null" {
			prs = []int64{is.Number}
		}
	case "check_run":
		ev.Check = &Check{}
		if c := p.CheckRun; c != nil {
			ev.Check = &Check{Name: checkName(c.Name), Conclusion: matched(conclusionRe, c.Conclusion)}
			ev.HeadSHA = matched(shaRe, c.HeadSHA)
			prs = numbers(c.PullRequests)
		}
	case "check_suite":
		ev.Check = &Check{}
		if c := p.CheckSuite; c != nil {
			ev.Check = &Check{Name: checkName(c.App.Name), Conclusion: matched(conclusionRe, c.Conclusion)}
			ev.HeadSHA = matched(shaRe, c.HeadSHA)
			prs = numbers(c.PullRequests)
		}
	case "status":
		ev.Check = &Check{Name: checkName(p.Context), Conclusion: matched(conclusionRe, p.State)}
		ev.HeadSHA = matched(shaRe, p.SHA)
	}
	ev.PRNumbers = prNumbers(prs)
	return ev, nil
}

func numbers(prs []prNumber) []int64 {
	out := make([]int64, 0, len(prs))
	for _, pr := range prs {
		out = append(out, pr.Number)
	}
	return out
}

// prNumbers keeps the positive numbers, ascending and once each, at most maxPRNumbers of them.
func prNumbers(in []int64) []int {
	out := []int{}
	for _, n := range in {
		if n > 0 && n <= 1<<31-1 {
			out = append(out, int(n))
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > maxPRNumbers {
		out = out[:maxPRNumbers]
	}
	return out
}

func matched(re *regexp.Regexp, s string) string {
	if re.MatchString(s) {
		return s
	}
	return ""
}

// refName keeps a branch name as it is or not at all: a name with a control, a format character or a space,
// or longer than maxBaseRef bytes, is no branch git allows.
func refName(s string) string {
	if s == "" || len(s) > maxBaseRef || !utf8.ValidString(s) {
		return ""
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return ""
		}
	}
	return s
}

// checkName cleans a check's name of invalid UTF-8, controls and format characters (bidi overrides
// included) and cuts it to MaxCheckName runes.
func checkName(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.ToValidUTF8(s, "") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == MaxCheckName {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

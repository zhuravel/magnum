package engine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
)

const fixRun = "run-fix-1"

// fixReportText is a report of fixRun built from its three bodies.
func fixReportText(pushed, replied, needs string) string {
	return agents.ReportMarker(fixRun) + "\n## Pushed\n" + pushed + "\n## Replied\n" + replied + "\n## Needs you\n" + needs + "\n"
}

func TestParseFixReportReadsTheThreeSections(t *testing.T) {
	text := fixReportText(
		"3fa9c1d Guard the nil owner in the lookup\nabcdef0123456789abcdef0123456789abcdef01 Rename the helper",
		"3 threads (2 fixed, 0 already fixed, 1 not a bug, 0 won't fix)",
		"- 2101 P1 app/models/offer.rb:42: the finding wants a schema change the author owns\n- 2102 P2 app/lib/x.rb:7: contested")
	got, err := ParseFixReport(text, fixRun)
	if err != nil {
		t.Fatalf("ParseFixReport: %v", err)
	}
	want := FixReport{
		Pushed: []FixCommit{
			{SHA: "3fa9c1d", Message: "Guard the nil owner in the lookup"},
			{SHA: "abcdef0123456789abcdef0123456789abcdef01", Message: "Rename the helper"},
		},
		Replied: "3 threads (2 fixed, 0 already fixed, 1 not a bug, 0 won't fix)",
		NeedsYou: []string{
			"2101 P1 app/models/offer.rb:42: the finding wants a schema change the author owns",
			"2102 P2 app/lib/x.rb:7: contested",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report = %+v\nwant     %+v", got, want)
	}
}

func TestParseFixReportReadsNothingPushedAndNoOneNeeded(t *testing.T) {
	got, err := ParseFixReport(fixReportText("nothing", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)", "none"), fixRun)
	if err != nil {
		t.Fatalf("ParseFixReport: %v", err)
	}
	if len(got.Pushed) != 0 || got.PushFailed || len(got.NeedsYou) != 0 || got.Replied == "" {
		t.Errorf("report = %+v, want nothing pushed, no push failure, nobody needed, the replied line kept", got)
	}
}

func TestParseFixReportReadsAFailedPush(t *testing.T) {
	got, err := ParseFixReport(fixReportText("push failed: ! [rejected] HEAD -> feature (non-fast-forward)", "0 threads (0 fixed, 0 already fixed, 0 not a bug, 0 won't fix)",
		"- 2101 P1 app/models/offer.rb:42: could not push"), fixRun)
	if err != nil {
		t.Fatalf("ParseFixReport: %v", err)
	}
	if !got.PushFailed || len(got.Pushed) != 0 {
		t.Errorf("report = %+v, want a failed push with no commits", got)
	}
	if len(got.NeedsYou) != 1 {
		t.Errorf("NeedsYou = %q, want the one entry", got.NeedsYou)
	}
}

func TestParseFixReportIsLenientAboutWhitespaceAndWhatFollows(t *testing.T) {
	marker := agents.ReportMarker(fixRun)
	cases := map[string]string{
		"blank lines between the sections":                  marker + "\n\n## Pushed\n\nnothing\n\n## Replied\n\n0 threads\n\n## Needs you\n\nnone\n",
		"no trailing newline":                               marker + "\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\nnone",
		"windows line endings":                              strings.ReplaceAll(marker+"\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\nnone\n", "\n", "\r\n"),
		"indented lines and a marker with blanks before it": "\n\n" + marker + "\n  ## Pushed  \n  nothing  \n## Replied\n  0 threads\n## Needs you\n  none\n",
		"text after the sections":                           marker + "\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\nnone\n\nDone. Anything else?\n",
		"another heading after the sections":                marker + "\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\n- 7 P2 a.rb:1: why\n## Notes\nfree text\n",
		"text before the first heading":                     marker + "\nFixed what I could.\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\nnone\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseFixReport(text, fixRun)
			if err != nil {
				t.Fatalf("ParseFixReport: %v", err)
			}
			if got.Replied != "0 threads" || len(got.Pushed) != 0 {
				t.Errorf("report = %+v", got)
			}
		})
	}
}

// A loose Markdown list puts a blank line between its items, and every item
// is a commit or a finding the author must hear about.
func TestParseFixReportReadsEveryItemOfALooseList(t *testing.T) {
	text := agents.ReportMarker(fixRun) + `
## Pushed
3fa9c1d Guard the nil owner

abcdef0 Rename the helper

## Replied
2 threads (2 fixed, 0 already fixed, 0 not a bug, 0 won't fix)

## Needs you

- 2101 P1 a.rb:1: first

- 2102 P2 b.rb:2: second
- 2103 P3 c.rb:3: third

Closing remark, not an entry.
- 9 P1 z.rb:9: after the remark
`
	got, err := ParseFixReport(text, fixRun)
	if err != nil {
		t.Fatalf("ParseFixReport: %v", err)
	}
	wantPushed := []FixCommit{{SHA: "3fa9c1d", Message: "Guard the nil owner"}, {SHA: "abcdef0", Message: "Rename the helper"}}
	if !reflect.DeepEqual(got.Pushed, wantPushed) {
		t.Errorf("Pushed = %+v, want %+v", got.Pushed, wantPushed)
	}
	wantNeeds := []string{"2101 P1 a.rb:1: first", "2102 P2 b.rb:2: second", "2103 P3 c.rb:3: third"}
	if !reflect.DeepEqual(got.NeedsYou, wantNeeds) {
		t.Errorf("NeedsYou = %q, want %q", got.NeedsYou, wantNeeds)
	}
}

func TestParseFixReportRefusesAPartialReport(t *testing.T) {
	marker := agents.ReportMarker(fixRun)
	cases := map[string]string{
		"Needs you missing":       marker + "\n## Pushed\nnothing\n## Replied\n0 threads\n",
		"Replied missing":         marker + "\n## Pushed\nnothing\n## Needs you\nnone\n",
		"Pushed missing":          marker + "\n## Replied\n0 threads\n## Needs you\nnone\n",
		"only the marker":         marker + "\n",
		"headings out of order":   marker + "\n## Pushed\nnothing\n## Needs you\nnone\n## Replied\n0 threads\n",
		"a heading twice":         marker + "\n## Pushed\nnothing\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\nnone\n",
		"an empty Pushed":         marker + "\n## Pushed\n## Replied\n0 threads\n## Needs you\nnone\n",
		"an empty Replied":        marker + "\n## Pushed\nnothing\n## Replied\n\n## Needs you\nnone\n",
		"an empty Needs you":      marker + "\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\n\n",
		"a heading of other case": marker + "\n## pushed\nnothing\n## Replied\n0 threads\n## Needs you\nnone\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFixReport(text, fixRun)
			if !errors.Is(err, ErrFixReportPartial) {
				t.Errorf("err = %v, want ErrFixReportPartial", err)
			}
			if errors.Is(err, ErrFixReportMarker) {
				t.Errorf("err = %v is also ErrFixReportMarker", err)
			}
		})
	}
}

func TestParseFixReportRefusesAReportOfAnotherRun(t *testing.T) {
	body := "\n## Pushed\nnothing\n## Replied\n0 threads\n## Needs you\nnone\n"
	cases := map[string]string{
		"another run's marker": agents.ReportMarker("run-fix-0") + body,
		"no marker":            body,
		"a marker mid-report":  "## Pushed\n" + agents.ReportMarker(fixRun) + body,
		"an empty report":      "",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFixReport(text, fixRun)
			if !errors.Is(err, ErrFixReportMarker) {
				t.Errorf("err = %v, want ErrFixReportMarker", err)
			}
		})
	}
	if _, err := ParseFixReport(agents.ReportMarker("")+body, ""); !errors.Is(err, ErrFixReportMarker) {
		t.Errorf("a report asked for no run: err = %v, want ErrFixReportMarker", err)
	}
}

func TestParseFixReportQuotesAMalformedLine(t *testing.T) {
	good := func(pushed, replied, needs string) string { return fixReportText(pushed, replied, needs) }
	cases := map[string]struct{ text, quoted string }{
		"a commit without a hash":      {good("fixed the thing", "0 threads", "none"), "fixed the thing"},
		"a hash too short":             {good("abc12 Short hash", "0 threads", "none"), "abc12 Short hash"},
		"nothing next to a commit":     {good("nothing\n3fa9c1d A commit", "0 threads", "none"), "3fa9c1d A commit"},
		"two replied lines":            {good("nothing", "1 threads (1 fixed)\nand a second line", "none"), "and a second line"},
		"a needs-you line, no bullet":  {good("nothing", "0 threads", "- 7 P2 a.rb:1: why\n7 P2 a.rb:2: no bullet"), "7 P2 a.rb:2: no bullet"},
		"none next to an entry":        {good("nothing", "0 threads", "none\n- 7 P2 a.rb:1: why"), "- 7 P2 a.rb:1: why"},
		"none, a blank line, an entry": {good("nothing", "0 threads", "none\n\n- 7 P2 a.rb:1: why"), "- 7 P2 a.rb:1: why"},
		"a bullet with nothing in it":  {good("nothing", "0 threads", "-"), "-"},
		"an unknown heading in Pushed": {good("3fa9c1d A commit\n## Notes", "0 threads", "none"), "## Notes"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseFixReport(c.text, fixRun)
			if err == nil {
				t.Fatal("ParseFixReport succeeded, want an error")
			}
			if errors.Is(err, ErrFixReportMarker) || errors.Is(err, ErrFixReportPartial) {
				t.Errorf("err = %v, want a plain error, not a sentinel", err)
			}
			if !strings.Contains(err.Error(), `"`+c.quoted+`"`) {
				t.Errorf("err = %q, want the line %q quoted", err, c.quoted)
			}
		})
	}
}

func TestParseFixReportClipsALongMalformedLine(t *testing.T) {
	long := strings.Repeat("x", 500)
	_, err := ParseFixReport(fixReportText(long, "0 threads", "none"), fixRun)
	if err == nil || len(err.Error()) > 300 {
		t.Errorf("err = %v (%d bytes), want a short error", err, len(err.Error()))
	}
}

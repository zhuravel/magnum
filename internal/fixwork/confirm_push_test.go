package fixwork

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/github"
)

// confirmScript is GitHub's side of a ConfirmPush: each read takes the
// next of answers (the last repeats) and readTakes of the clock, each wait
// moves the clock by its length (unless the clock stands still) and then
// runs onWait, and the log keeps the reads, the waits and the marks in
// their order.
type confirmScript struct {
	answers   []confirmAnswer
	readTakes time.Duration
	still     bool
	onRead    func()
	onWait    func(ctx context.Context) error
	now       time.Time
	reads     int
	log       []string
}

// confirmAnswer is one read of the PR's head on GitHub.
type confirmAnswer struct {
	head string
	err  error
}

// confirm is the PushConfirm that runs s.
func (s *confirmScript) confirm() PushConfirm {
	return PushConfirm{
		Read: func(context.Context) (string, error) {
			a := s.answers[min(s.reads, len(s.answers)-1)]
			s.reads++
			s.now = s.now.Add(s.readTakes)
			s.log = append(s.log, "read")
			if s.onRead != nil {
				s.onRead()
			}
			return a.head, a.err
		},
		Now: func() time.Time { return s.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			s.log = append(s.log, "wait "+d.String())
			if !s.still {
				s.now = s.now.Add(d)
			}
			if s.onWait != nil {
				return s.onWait(ctx)
			}
			return nil
		},
		Mark:   func() { s.log = append(s.log, "mark") },
		Unmark: func() { s.log = append(s.log, "unmark") },
	}
}

// wantLog fails unless s logged want.
func (s *confirmScript) wantLog(t *testing.T, want ...string) {
	t.Helper()
	if !slices.Equal(s.log, want) {
		t.Errorf("the confirmation did %q, want %q", s.log, want)
	}
}

// GitHub shows the pushed head at the first read: the push is confirmed
// with no wait, and the loop's row is never marked or cleared.
func TestAPushGitHubShowsAtOnceIsConfirmedWithNoWaitAndNoMark(t *testing.T) {
	s := &confirmScript{answers: []confirmAnswer{{head: fixPushed}}}
	if err := ConfirmPush(t.Context(), s.confirm(), fixPushed); err != nil {
		t.Fatalf("ConfirmPush = %v, want nil", err)
	}
	s.wantLog(t, "read")
}

// GitHub moves a PR's head a moment after the push to its branch: a head
// it shows otherwise is looked at again 5 seconds later, and the pushed
// head then confirms the push. GitHub answered each time, so the row is
// never marked.
func TestAnotherHeadThenThePushedOneConfirmsThePushAfterLookingAgain(t *testing.T) {
	s := &confirmScript{answers: []confirmAnswer{{head: fixHead}, {head: fixHead}, {head: fixPushed}}}
	if err := ConfirmPush(t.Context(), s.confirm(), fixPushed); err != nil {
		t.Fatalf("ConfirmPush = %v, want nil", err)
	}
	s.wantLog(t, "read", "wait 5s", "read", "wait 5s", "read")
}

// A head GitHub still shows otherwise at the third look fails the push as
// not confirmed, naming both heads by their short SHAs.
func TestAnotherHeadAtEveryLookFailsThePushAsNotConfirmed(t *testing.T) {
	s := &confirmScript{answers: []confirmAnswer{{head: fixHead}}}
	err := ConfirmPush(t.Context(), s.confirm(), fixPushed)
	p, ok := errors.AsType[PushNotConfirmed](err)
	if !ok || p.Head != fixPushed {
		t.Fatalf("ConfirmPush = %#v, want PushNotConfirmed of %s", err, fixPushed)
	}
	if want := "push not confirmed on GitHub: the PR's head is a1a1a1a, not the pushed b2b2b2b"; err.Error() != want {
		t.Errorf("the error says %q, want %q", err, want)
	}
	s.wantLog(t, "read", "wait 5s", "read", "wait 5s", "read")
}

// A read GitHub or the network failed to answer (gh could not connect) is
// tried again after the backoff's first wait, and the pushed head then
// confirms the push.
func TestATransientReadThenAGoodOneConfirmsThePush(t *testing.T) {
	s := &confirmScript{answers: []confirmAnswer{{err: ghDown()}, {head: fixPushed}}}
	if err := ConfirmPush(t.Context(), s.confirm(), fixPushed); err != nil {
		t.Fatalf("ConfirmPush = %v, want nil", err)
	}
	s.wantLog(t, "read", "mark", "wait 5s", "read", "unmark")
}

// GitHub's own answer (a 403) is no blip: the push fails as not confirmed
// at once with that error, with no wait and no mark.
func TestANonTransientReadFailsThePushAtOnce(t *testing.T) {
	forbidden := &github.APIError{Op: "details talkable/talkable (2)", Status: 403, Message: "Resource not accessible by integration"}
	s := &confirmScript{answers: []confirmAnswer{{err: forbidden}, {head: fixPushed}}}
	err := ConfirmPush(t.Context(), s.confirm(), fixPushed)
	if p, ok := errors.AsType[PushNotConfirmed](err); !ok || p.Head != fixPushed || !errors.Is(err, forbidden) {
		t.Fatalf("ConfirmPush = %#v, want PushNotConfirmed of %s with the 403", err, fixPushed)
	}
	if want := "push not confirmed on GitHub: " + forbidden.Error(); err.Error() != want {
		t.Errorf("the error says %q, want %q", err, want)
	}
	s.wantLog(t, "read")
}

// GitHub that does not answer within the window leaves the push
// unanswered, for the ticks to try again, with the last failure: after the
// backoff's six waits (3 minutes in all) and seven reads; after 3 minutes
// when the reads are slow; and after the six waits even when the clock
// stands still. The row stays marked: the settle still confirms the push.
func TestGitHubThatDoesNotAnswerWithinTheWindowLeavesThePushUnanswered(t *testing.T) {
	waits := []string{"wait 5s", "wait 10s", "wait 20s", "wait 40s", "wait 1m0s", "wait 45s"}
	for _, tc := range []struct {
		name      string
		readTakes time.Duration
		still     bool
		want      []string
	}{
		{name: "the backoff runs out", want: waits},
		{name: "slow reads", readTakes: time.Minute, want: waits[:2]},
		{name: "a clock that stands still", still: true, want: waits},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var answers []confirmAnswer
			for i := range len(waits) + 1 {
				answers = append(answers, confirmAnswer{err: []error{ghDown(), gitDown()}[i%2]})
			}
			s := &confirmScript{answers: answers, readTakes: tc.readTakes, still: tc.still}
			err := ConfirmPush(t.Context(), s.confirm(), fixPushed)
			last := answers[len(tc.want)].err
			p, ok := errors.AsType[PushUnanswered](err)
			if !ok || p.Head != fixPushed || !errors.Is(err, last) {
				t.Fatalf("ConfirmPush = %#v, want PushUnanswered of %s with the last failure", err, fixPushed)
			}
			if want := "push not confirmed on GitHub yet: GitHub does not answer: " + last.Error(); err.Error() != want {
				t.Errorf("the error says %q, want %q", err, want)
			}
			want := []string{"read", "mark"}
			for _, w := range tc.want {
				want = append(want, w, "read")
			}
			s.wantLog(t, want...)
		})
	}
}

// A stop during a wait ends the confirmation at once with the context's
// error: no read follows, and the push is neither unanswered nor not
// confirmed, so a shutdown or a drain never waits for GitHub. A stop
// during a read ends it the same way, whatever the read returned.
func TestAStopEndsTheConfirmationAtOnceWithTheContextsError(t *testing.T) {
	t.Run("during a wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s := &confirmScript{answers: []confirmAnswer{{err: ghDown()}, {head: fixPushed}}, onWait: func(ctx context.Context) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}}
		err := ConfirmPush(ctx, s.confirm(), fixPushed)
		if !errors.Is(err, context.Canceled) || isPushVerdict(err) {
			t.Fatalf("ConfirmPush = %#v, want the context's error alone", err)
		}
		s.wantLog(t, "read", "mark", "wait 5s")
	})
	t.Run("during a read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s := &confirmScript{answers: []confirmAnswer{{err: errors.New("gh: signal: killed")}}, onRead: cancel}
		err := ConfirmPush(ctx, s.confirm(), fixPushed)
		if !errors.Is(err, context.Canceled) || isPushVerdict(err) {
			t.Fatalf("ConfirmPush = %#v, want the context's error alone", err)
		}
		s.wantLog(t, "read")
	})
}

// isPushVerdict reports whether err is one of ConfirmPush's verdicts on
// the push rather than a stop.
func isPushVerdict(err error) bool {
	_, unconfirmed := errors.AsType[PushNotConfirmed](err)
	_, unanswered := errors.AsType[PushUnanswered](err)
	return unconfirmed || unanswered
}

// The loop's row says that its settle confirms the push from the first
// transient failure, once however many follow and not for a head GitHub
// shows otherwise, until GitHub answers with the pushed head: the mark
// comes before the first wait of the backoff, the clearing after the read
// that answered.
func TestTheRowIsMarkedAtTheFirstTransientFailureAndClearedOnceGitHubAnswers(t *testing.T) {
	s := &confirmScript{answers: []confirmAnswer{{head: fixHead}, {err: ghDown()}, {err: gitDown()}, {head: fixPushed}}}
	if err := ConfirmPush(t.Context(), s.confirm(), fixPushed); err != nil {
		t.Fatalf("ConfirmPush = %v, want nil", err)
	}
	s.wantLog(t, "read", "wait 5s", "read", "mark", "wait 5s", "read", "wait 10s", "read", "unmark")
}

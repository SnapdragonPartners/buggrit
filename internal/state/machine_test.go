package state

import (
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

func TestNext(t *testing.T) {
	fixed := &Verdict{State: StateFixed, Reason: "initialized in New", Evidence: []string{"a.go"}}
	present := &Verdict{State: StateOpen, Reason: "still nil"}

	type tc struct {
		v        *Verdict
		name     string
		state    FindingState
		want     Step
		resolved bool
		settled  bool
		unknown  bool
	}
	for _, c := range []tc{
		{name: "open, fixed", v: fixed, want: Step{Kind: KindFixed, Reason: fixed.Reason, Evidence: fixed.Evidence, Want: Resolve}},
		{name: "open, not verified", want: Step{Want: Unresolve}},
		{name: "open, still present: no reply", v: present, want: Step{Want: Unresolve}},
		{name: "open, resolved by buggrit: unresolve without a reply", resolved: true, v: present, want: Step{Want: Unresolve}},
		{name: "open, resolved by buggrit, not verified: unresolve", resolved: true, want: Step{Want: Unresolve}},
		{name: "open, resolver unknown: never unresolved", resolved: true, unknown: true, v: present, want: Step{}},
		{name: "open, resolver unknown, fixed", resolved: true, unknown: true, v: fixed,
			want: Step{Kind: KindFixed, Reason: fixed.Reason, Evidence: fixed.Evidence, Want: Resolve}},

		{name: "settled open finding is left alone", resolved: true, settled: true, v: fixed, want: Step{}},
		{name: "settled fixed finding is left alone", state: StateFixed, resolved: true, settled: true, v: present, want: Step{}},
		{name: "settled dismissed finding is left alone", state: StateDismissed, resolved: true, settled: true, want: Step{}},

		{name: "fixed, not re-verified: repair", state: StateFixed, want: Step{Want: Resolve}},
		{name: "fixed, still fixed: nothing to say", state: StateFixed, resolved: true, v: fixed, want: Step{Want: Resolve}},
		{name: "regression", state: StateFixed, resolved: true, v: present, want: Step{Kind: KindRegressed, Reason: present.Reason, Want: Unresolve}},
		{name: "regression, resolver unknown: reply, leave resolved", state: StateFixed, resolved: true, unknown: true, v: present,
			want: Step{Kind: KindRegressed, Reason: present.Reason}},

		{name: "dismissed is terminal", state: StateDismissed, v: present, want: Step{Want: Resolve}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := &Tracked{
				Thread: &github.Thread{IsResolved: c.resolved}, ID: "f-1", Severity: "P1", State: StateOpen,
				Settled: c.settled, ResolverUnknown: c.unknown,
			}
			if c.state != "" {
				tr.State = c.state
			}
			got := Next(tr, c.v)
			if got.Kind != c.want.Kind || got.Reason != c.want.Reason || got.Want != c.want.Want || len(got.Evidence) != len(c.want.Evidence) {
				t.Errorf("Next = %+v, want %+v", got, c.want)
			}
		})
	}
}

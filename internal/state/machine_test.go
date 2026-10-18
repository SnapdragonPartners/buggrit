package state

import (
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

func TestNext(t *testing.T) {
	fixed := &Verdict{State: StateFixed, Reason: "initialized in New", Evidence: []string{"a.go"}}
	present := &Verdict{State: StateOpen, Reason: "still nil"}
	valid := &Verdict{State: StateDismissed, Reason: "intentional, see ADR"}
	reply := []github.Comment{human(9, "why")}
	rebutted := &Marker{ID: "f-1", To: StateOpen, Kind: KindRebutted, SHA: "a"}
	reopened := &Marker{ID: "f-1", To: StateOpen, Kind: KindReopened, SHA: "a"}

	type tc struct {
		last      *Marker
		v         *Verdict
		name      string
		state     FindingState
		sev       string
		pending   []github.Comment
		want      Step
		rebuttals int
		resolved  bool
	}
	for _, c := range []tc{
		{name: "open, fixed", v: fixed, want: Step{Kind: KindFixed, Reason: fixed.Reason, Evidence: fixed.Evidence, Want: Resolve}},
		{name: "open, not verified", want: Step{}},
		{name: "open, still present, nobody touched it", v: present, want: Step{}},
		{name: "false resolve reopens", resolved: true, v: present, want: Step{Kind: KindReopened, Reason: present.Reason, Want: Unresolve}},
		{name: "resolved again after a reopen: just unresolve", resolved: true, last: reopened, v: present, want: Step{Want: Unresolve}},
		{name: "resolved after a rebuttal: just unresolve", resolved: true, last: rebutted, v: present, want: Step{Want: Unresolve}},
		{name: "resolved after a fix the verifier now doubts", resolved: true, v: fixed, want: Step{Kind: KindFixed, Reason: fixed.Reason, Evidence: fixed.Evidence, Want: Resolve}},
		{name: "valid rebuttal", pending: reply, v: valid, want: Step{Kind: KindDismissed, Reason: valid.Reason, Want: Resolve}},
		{name: "invalid rebuttal", pending: reply, v: present, want: Step{Kind: KindRebutted, Reason: present.Reason, Want: Unresolve}},
		{name: "invalid rebuttal, resolved by the replier", pending: reply, resolved: true, v: present, want: Step{Kind: KindRebutted, Reason: present.Reason, Want: Unresolve}},
		{name: "rebuttals used up", pending: reply, rebuttals: 1, v: present, want: Step{Kind: KindNeedsHuman, Reason: present.Reason, Want: Unresolve}},
		{name: "advisory reply dismisses", sev: "P2", pending: reply, v: present, want: Step{Kind: KindDismissed, Reason: present.Reason, Want: Resolve}},
		{name: "advisory reply with a fix", sev: "P3", pending: reply, v: fixed, want: Step{Kind: KindFixed, Reason: fixed.Reason, Evidence: fixed.Evidence, Want: Resolve}},
		{name: "advisory false resolve still reopens", sev: "P2", resolved: true, v: present, want: Step{Kind: KindReopened, Reason: present.Reason, Want: Unresolve}},

		{name: "needs_human settled by a reply", state: StateNeedsHuman, pending: reply, want: Step{Kind: KindSettled, Want: Resolve}},
		{name: "needs_human settled by a resolution", state: StateNeedsHuman, resolved: true, want: Step{Kind: KindSettled, Want: Resolve}},
		{name: "needs_human, fixed meanwhile", state: StateNeedsHuman, v: fixed, want: Step{Kind: KindFixed, Reason: fixed.Reason, Evidence: fixed.Evidence, Want: Resolve}},
		{name: "needs_human waits", state: StateNeedsHuman, v: present, want: Step{}},

		{name: "fixed, not re-verified: repair", state: StateFixed, want: Step{Want: Resolve}},
		{name: "fixed, still fixed: nothing to say", state: StateFixed, v: fixed, want: Step{Want: Resolve}},
		{name: "fixed, reply acknowledged", state: StateFixed, pending: reply, v: fixed, want: Step{Kind: KindFixed, Reason: fixed.Reason, Evidence: fixed.Evidence, Want: Resolve}},
		{name: "regression", state: StateFixed, resolved: true, v: present, want: Step{Kind: KindRegressed, Reason: present.Reason, Want: Unresolve}},
		{name: "fixed, reply that holds up", state: StateFixed, pending: reply, v: valid, want: Step{Kind: KindDismissed, Reason: valid.Reason, Want: Resolve}},

		{name: "dismissed is terminal", state: StateDismissed, pending: reply, v: present, want: Step{Want: Resolve}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := &Tracked{
				Thread: &github.Thread{IsResolved: c.resolved}, ID: "f-1", Severity: "P1", State: StateOpen,
				Last: c.last, Pending: c.pending, Rebuttals: c.rebuttals,
			}
			if c.state != "" {
				tr.State = c.state
			}
			if c.sev != "" {
				tr.Severity = c.sev
			}
			got := Next(tr, c.v, 1)
			if got.Kind != c.want.Kind || got.Reason != c.want.Reason || got.Want != c.want.Want || len(got.Evidence) != len(c.want.Evidence) {
				t.Errorf("Next = %+v, want %+v", got, c.want)
			}
		})
	}
}

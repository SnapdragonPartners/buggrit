package state

// Verdict is the verifier's judgment of one finding (design §6, "Verdicts
// carry evidence"): StateFixed, with Evidence, or StateOpen.
type Verdict struct {
	State    FindingState
	Reason   string
	Evidence []string
}

// Want is the resolved state a thread should end up in.
type Want int

// Thread resolution after a step.
const (
	Keep Want = iota
	Resolve
	Unresolve
)

// Step is what a pass does with one finding: an optional transition, which
// buggrit posts as a reply carrying its marker, and the thread resolution to
// reach. A step with no Kind is a repair at most: the resolution is fixed
// without posting anything (§6, "Repair every pass").
type Step struct {
	Kind     Kind
	Reason   string
	Evidence []string
	Want     Want
}

// Next applies the state machine in §6 to a finding. v is the verifier's
// verdict, or nil if the finding wasn't verified this pass (not selected,
// or verification didn't reach it).
//
// buggrit posts only on transitions, fixed and regressed. A settled finding
// is left alone. Otherwise buggrit owns the thread's resolution: a fixed
// finding's thread is kept resolved, and an open finding's thread that
// buggrit resolved is unresolved, which repairs a run killed between its
// regressed reply and its unresolve. A thread whose resolver GitHub doesn't
// report is never unresolved, since a person may have settled it.
func Next(t *Tracked, v *Verdict) Step {
	var s Step
	switch {
	case t.Settled:
		return Step{}
	case t.State == StateDismissed:
		return Step{Want: Resolve}
	case t.State == StateFixed:
		s.Want = Resolve
		if v != nil && v.State == StateOpen {
			s = Step{Kind: KindRegressed, Reason: v.Reason, Want: Unresolve}
		}
	case v != nil && v.State == StateFixed:
		return Step{Kind: KindFixed, Reason: v.Reason, Evidence: v.Evidence, Want: Resolve}
	default:
		s.Want = Unresolve
	}
	if s.Want == Unresolve && t.ResolverUnknown {
		s.Want = Keep
	}
	return s
}

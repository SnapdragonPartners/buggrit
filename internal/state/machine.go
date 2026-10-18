package state

// Verdict is the verifier's judgment of one finding (design §6, "Verdicts
// carry evidence"). State is StateFixed (with Evidence), StateOpen (still
// present, or a reply that doesn't hold up), or StateDismissed (a reply
// that holds up).
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

// NeedsSettling reports whether a needs_human finding has been settled by a
// human: any reply or resolution since it went to a human (§6, "Humans
// settle findings"). Settling needs no model.
func (t *Tracked) NeedsSettling() bool {
	return t.State == StateNeedsHuman && (len(t.Pending) > 0 || t.Thread.IsResolved)
}

// Next applies the state machine in §6 to a finding. v is the verifier's
// verdict, or nil if the finding wasn't verified this pass (not selected,
// or verification didn't reach it). maxRebuttals is convergence.max_rebuttals.
//
// A transition the thread already shows is never posted again: with no
// human activity since buggrit last said the finding is open, a resolved
// thread is just unresolved. That makes a run killed between its reply and
// its unresolve harmless, and stops buggrit repeating itself when someone
// resolves the thread again without a word.
func Next(t *Tracked, v *Verdict, maxRebuttals int) Step {
	switch t.State {
	case StateDismissed:
		return Step{Want: Resolve}
	case StateNeedsHuman:
		switch {
		case t.NeedsSettling():
			return Step{Kind: KindSettled, Want: Resolve}
		case v != nil && v.State == StateFixed:
			return Step{Kind: KindFixed, Reason: v.Reason, Evidence: v.Evidence, Want: Resolve}
		}
		return Step{}
	case StateFixed:
		return nextFixed(t, v)
	}
	return nextOpen(t, v, maxRebuttals)
}

func nextFixed(t *Tracked, v *Verdict) Step {
	if v == nil {
		return Step{Want: Resolve}
	}
	switch v.State {
	case StateOpen:
		return Step{Kind: KindRegressed, Reason: v.Reason, Want: Unresolve}
	case StateDismissed:
		if len(t.Pending) > 0 {
			return Step{Kind: KindDismissed, Reason: v.Reason, Want: Resolve}
		}
	case StateFixed:
		// Still fixed. A reply is acknowledged with a fresh fixed
		// transition, which also moves the evidence baseline to this head;
		// without one there's nothing to say.
		if len(t.Pending) > 0 {
			return Step{Kind: KindFixed, Reason: v.Reason, Evidence: v.Evidence, Want: Resolve}
		}
	}
	return Step{Want: Resolve}
}

func nextOpen(t *Tracked, v *Verdict, maxRebuttals int) Step {
	if v == nil {
		return Step{}
	}
	replied := len(t.Pending) > 0
	switch {
	case v.State == StateFixed:
		return Step{Kind: KindFixed, Reason: v.Reason, Evidence: v.Evidence, Want: Resolve}
	case v.State == StateDismissed && replied,
		// §7 rule 4: for an advisory finding, the agent's reply decides.
		replied && !t.Blocking():
		return Step{Kind: KindDismissed, Reason: v.Reason, Want: Resolve}
	case replied && t.Rebuttals < maxRebuttals:
		return Step{Kind: KindRebutted, Reason: v.Reason, Want: Unresolve}
	case replied:
		return Step{Kind: KindNeedsHuman, Reason: v.Reason, Want: Unresolve}
	case !t.Thread.IsResolved:
		return Step{}
	case t.Last != nil && t.Last.Kind.To() == StateOpen:
		// buggrit already said this is open, and nobody has replied since.
		return Step{Want: Unresolve}
	}
	// Resolved without a reply while the issue is still present.
	return Step{Kind: KindReopened, Reason: v.Reason, Want: Unresolve}
}

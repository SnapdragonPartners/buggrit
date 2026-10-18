package review

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// Transition is what a pass does with one of buggrit's earlier findings: a
// state transition to post, a resolve or unresolve to repair, or both
// (design §6). Publishing renders and posts it.
type Transition struct {
	Tracked *state.Tracked
	state.Step
	// Seen is the newest human comment in the thread when the step was
	// decided; the transition's marker records it.
	Seen int64
}

// Mutates reports whether the transition changes anything on GitHub.
func (t *Transition) Mutates() bool { return t.Kind != "" || t.Repair() }

// Repair reports whether the thread's resolution must change.
func (t *Transition) Repair() bool {
	resolved := t.Tracked.Thread.IsResolved
	return t.Want == state.Resolve && !resolved || t.Want == state.Unresolve && resolved
}

// selectChecks picks the earlier findings to verify this pass (design §6,
// "What gets verified each pass"):
//   - every open finding, resolved or not, since resolving isn't proof;
//   - every needs_human finding that no human has settled yet;
//   - every fixed finding with a new reply, whose fix commit is no longer in
//     the head's history (a force-push), or any of whose evidence files
//     changed since the fix.
//
// Dismissed findings are terminal, and a settled needs_human finding is
// dismissed without a model.
func selectChecks(ctx context.Context, gh GitHub, ref PRRef, tracked []state.Tracked) []Check {
	since := map[string]fixDelta{}
	fixed := func(sha string) fixDelta {
		d, ok := since[sha]
		if !ok {
			d = changedSince(ctx, gh, ref, sha)
			since[sha] = d
		}
		return d
	}
	var out []Check
	for i := range tracked {
		t := &tracked[i]
		if why := checkReason(t, fixed); why != "" {
			out = append(out, Check{Tracked: t, Why: why})
		}
	}
	return out
}

// checkReason says why t needs verifying, or "" if it doesn't. fixed reads
// what changed since a fix commit.
func checkReason(t *state.Tracked, fixed func(sha string) fixDelta) string {
	switch {
	case t.State == state.StateOpen && len(t.Pending) > 0:
		return "it has a new reply"
	case t.State == state.StateOpen && t.Thread.IsResolved:
		return "it is open but its thread was resolved"
	case t.State == state.StateOpen:
		return "it is open"
	case t.State == state.StateNeedsHuman && !t.NeedsSettling():
		return "it is waiting for a human; check whether it has been fixed meanwhile"
	case t.State == state.StateFixed && len(t.Pending) > 0:
		return "it was fixed, and has a new reply"
	case t.State == state.StateFixed:
		return fixed(t.FixSHA).why(t)
	}
	return ""
}

// fixDelta is what changed between a fix commit and the head.
type fixDelta struct {
	changed map[string]bool
	// unknown says why the changes couldn't be read; every finding fixed
	// at that commit is then verified again.
	unknown string
	fix     string
}

// changedSince reads the files changed from fix to the head. Anything it
// can't establish (a fix commit no longer in the head's history, an API
// error, a diff too large to render) means verifying again, which is
// always safe.
func changedSince(ctx context.Context, gh GitHub, ref PRRef, fix string) fixDelta {
	d := fixDelta{fix: fix, changed: map[string]bool{}}
	if fix == ref.HeadSHA {
		return d
	}
	status, err := gh.CompareStatus(ctx, ref.Repo, fix, ref.HeadSHA)
	switch {
	case err != nil && github.IsNotFound(err):
		d.unknown = "the commit it was fixed at, " + short(fix) + ", is gone from the repository (a force-push)"
		return d
	case err != nil:
		d.unknown = "the changes since its fix at " + short(fix) + " could not be read"
		return d
	case status != github.CompareAhead && status != github.CompareIdentical:
		d.unknown = "the commit it was fixed at, " + short(fix) + ", is no longer in the branch's history (a force-push or rebase)"
		return d
	}
	diff, err := gh.CompareDiff(ctx, ref.Repo, fix, ref.HeadSHA)
	if err != nil {
		d.unknown = "the changes since its fix at " + short(fix) + " could not be read"
		return d
	}
	for i := range diff.Files {
		f := &diff.Files[i]
		for _, p := range []string{f.OldPath, f.NewPath} {
			if p != "" {
				d.changed[p] = true
			}
		}
	}
	return d
}

// why says why a fixed finding needs verifying again, or "" if it doesn't:
// none of the files its verdict relied on changed, so the verdict still
// holds at the head (§6, "Why a late transition from an older head is
// safe").
func (d fixDelta) why(t *state.Tracked) string {
	if d.unknown != "" {
		return "it was fixed, but " + d.unknown
	}
	var changed []string
	for _, p := range t.Evidence {
		if d.changed[p] {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return ""
	}
	return fmt.Sprintf("it was fixed at %s, and the files that verdict relied on have changed since: %s", short(d.fix), strings.Join(changed, ", "))
}

// transitions applies the state machine to every earlier finding, given the
// verdicts this pass reached (nil for none), and returns the ones that
// change something on GitHub.
func transitions(tracked []state.Tracked, checks []Check, verdicts map[string]*state.Verdict, maxRebuttals int) []Transition {
	var out []Transition
	for i := range tracked {
		t := &tracked[i]
		var v *state.Verdict
		if slices.ContainsFunc(checks, func(c Check) bool { return c.Tracked == t }) {
			v = verdicts[t.ID]
		}
		tr := Transition{Tracked: t, Step: state.Next(t, v, maxRebuttals), Seen: newestHuman(t)}
		if tr.Mutates() {
			out = append(out, tr)
		}
	}
	return out
}

// newestHuman is the database ID of the newest human comment in t's thread:
// the latest seen by this pass, since the thread was read before deciding.
func newestHuman(t *state.Tracked) int64 {
	n := t.Seen
	for i := range t.Pending {
		n = max(n, t.Pending[i].DatabaseID)
	}
	return n
}

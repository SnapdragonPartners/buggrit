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
//   - every open finding that isn't settled;
//   - every fixed finding that isn't settled, if any of the files its
//     evidence baseline relied on changed since that baseline's commit, or
//     no baseline commit is still in the head's history (a force-push).
//
// A settled finding is never verified, and neither is a dismissed one.
func selectChecks(ctx context.Context, gh GitHub, ref PRRef, tracked []state.Tracked, b *state.Baseline) []Check {
	h := &history{ctx: ctx, gh: gh, ref: ref, status: map[string]ancestry{}, diffs: map[string]fixDelta{}}
	var out []Check
	for i := range tracked {
		t := &tracked[i]
		if why := checkReason(t, b, h); why != "" {
			out = append(out, Check{Tracked: t, Why: why})
		}
	}
	return out
}

// checkReason says why t needs verifying, or "" if it doesn't.
func checkReason(t *state.Tracked, b *state.Baseline, h *history) string {
	switch {
	case t.Settled:
		return ""
	case t.State == state.StateOpen:
		return "it is open"
	case t.State == state.StateFixed:
		return h.why(b.FixRecords(t))
	}
	return ""
}

// history reads, and caches, how earlier commits relate to the head.
type history struct {
	ctx    context.Context //nolint:containedctx // scoped to one selectChecks call
	gh     GitHub
	status map[string]ancestry
	diffs  map[string]fixDelta
	ref    PRRef
}

// ancestry is whether a commit is in the head's history; unknown says why
// that couldn't be read.
type ancestry struct {
	unknown string
	in      bool
}

func (h *history) ancestry(sha string) ancestry {
	if a, ok := h.status[sha]; ok {
		return a
	}
	a := ancestry{in: sha == h.ref.HeadSHA}
	if a.in {
		h.status[sha] = a
		return a
	}
	status, err := h.gh.CompareStatus(h.ctx, h.ref.Repo, sha, h.ref.HeadSHA)
	switch {
	case err != nil && github.IsNotFound(err):
		// The commit is gone from the repository: a force-push.
	case err != nil:
		a.unknown = "the changes since " + short(sha) + " could not be read"
	default:
		a.in = status == github.CompareAhead || status == github.CompareIdentical
	}
	h.status[sha] = a
	return a
}

// why says why a fixed finding needs verifying again, given its fix
// records newest first, or "" if it doesn't. Its evidence baseline is the
// newest record whose commit is in the head's history. If none of the
// files that record relied on changed since, the verdict still holds at the
// head (§6, "Why a late transition from an older head is safe").
func (h *history) why(records []state.Record) string {
	for i := range records {
		r := &records[i]
		a := h.ancestry(r.SHA)
		switch {
		case a.unknown != "":
			return "it was fixed, but " + a.unknown
		case !a.in:
			continue
		}
		d := h.changed(r.SHA)
		if d.unknown != "" {
			return "it was fixed, but " + d.unknown
		}
		var changed []string
		for _, p := range r.Files {
			if d.changed[p] {
				changed = append(changed, p)
			}
		}
		if len(changed) == 0 {
			return ""
		}
		return fmt.Sprintf("it was last verified fixed at %s, and the files that verdict relied on have changed since: %s",
			short(r.SHA), strings.Join(changed, ", "))
	}
	return "it was fixed, but no commit it was verified at is still in the branch's history (a force-push or rebase)"
}

// fixDelta is what changed between an earlier commit and the head.
type fixDelta struct {
	changed map[string]bool
	// unknown says why the changes couldn't be read; the finding is then
	// verified again.
	unknown string
}

// changed reads the files changed from sha, which is in the head's
// history, to the head. Anything it can't establish (an API error, a diff
// too large to render) means verifying again, which is always safe.
func (h *history) changed(sha string) fixDelta {
	if d, ok := h.diffs[sha]; ok {
		return d
	}
	d := fixDelta{changed: map[string]bool{}}
	if sha != h.ref.HeadSHA {
		diff, err := h.gh.CompareDiff(h.ctx, h.ref.Repo, sha, h.ref.HeadSHA)
		if err != nil {
			d.unknown = "the changes since " + short(sha) + " could not be read"
		} else {
			for i := range diff.Files {
				f := &diff.Files[i]
				for _, p := range []string{f.OldPath, f.NewPath} {
					if p != "" {
						d.changed[p] = true
					}
				}
			}
		}
	}
	h.diffs[sha] = d
	return d
}

// transitions applies the state machine to every earlier finding, given the
// verdicts this pass reached (nil for none), and returns the ones that
// change something on GitHub.
func transitions(tracked []state.Tracked, checks []Check, verdicts map[string]*state.Verdict) []Transition {
	var out []Transition
	for i := range tracked {
		t := &tracked[i]
		var v *state.Verdict
		if slices.ContainsFunc(checks, func(c Check) bool { return c.Tracked == t }) {
			v = verdicts[t.ID]
		}
		tr := Transition{Tracked: t, Step: state.Next(t, v)}
		if tr.Mutates() {
			out = append(out, tr)
		}
	}
	return out
}

// reverified lists the fixed findings this pass verified as still fixed,
// with the evidence files of that verdict. The pass marker records them,
// since a still-fixed finding gets no reply (§6, "Evidence baseline").
func reverified(checks []Check, verdicts map[string]*state.Verdict) map[string][]string {
	out := map[string][]string{}
	for i := range checks {
		t := checks[i].Tracked
		if v := verdicts[t.ID]; t.State == state.StateFixed && v != nil && v.State == state.StateFixed {
			out[t.ID] = v.Evidence
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

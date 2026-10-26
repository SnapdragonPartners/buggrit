package state

import (
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// Tracked is one of buggrit's findings, as replayed from its thread (design
// §6, "The thread is the source of truth").
type Tracked struct {
	Thread *github.Thread
	// Root is the thread's first comment, which carries the finding marker.
	Root *github.Comment
	// Last is the latest transition; nil while the finding has none.
	Last *Marker
	ID   string
	FP   string
	// Severity is the finding's severity when it was raised.
	Severity string
	// RaisedSHA is the head the finding was raised at.
	RaisedSHA string
	State     FindingState
	// Fix is the latest fixed transition: the head the fix was verified
	// at, the files the verdict relied on, and when it was posted. It's set
	// only while State is fixed.
	Fix *Record
	// Own is buggrit's own comments on the finding, root first: what the
	// verifier may read of the thread. Nobody else's comments reach it.
	Own []github.Comment
	// Settled is set when the thread is resolved by someone other than
	// buggrit. That settles the finding whatever its markers say: it's
	// never verified, reopened or counted again (§6).
	Settled bool
	// ResolverUnknown is set when the thread is resolved but GitHub doesn't
	// say by whom (a deleted account). The finding isn't settled, but
	// buggrit never unresolves the thread either.
	ResolverUnknown bool
}

// Record is a verdict that a finding is fixed: the head it was verified
// at, the files it relied on, and when buggrit posted it. A fixed finding's
// evidence baseline is one of these (§6).
type Record struct {
	At    time.Time
	SHA   string
	Files []string
}

// Blocking reports whether the finding blocks convergence (P0 or P1, §5).
func (t *Tracked) Blocking() bool { return t.Severity == "P0" || t.Severity == "P1" }

// Replay reads buggrit's findings from a PR's threads. A thread is a finding
// if its root comment carries a finding marker and is buggrit's own (§3);
// other threads belong to other reviewers. Within a thread, only buggrit's
// own transition markers for that finding count, in order. Everyone else's
// comments carry no state: a finding is settled by resolving its thread,
// and the thread's resolver says who did (§6).
func Replay(threads []github.Thread, botLogin string) []Tracked {
	out := make([]Tracked, 0, len(threads))
	for i := range threads {
		th := &threads[i]
		root := th.Root()
		if root == nil {
			continue
		}
		m, own := OwnMarker(root, botLogin)
		if !own || m.FP == "" || m.IsTransition() || m.IsPass() {
			continue
		}
		t := Tracked{
			Thread: th, Root: root, ID: m.ID, FP: m.FP, Severity: m.Severity, RaisedSHA: m.SHA, State: StateOpen,
			Own:             []github.Comment{*root},
			Settled:         th.IsResolved && th.ResolvedBy != "" && !SameLogin(th.ResolvedBy, botLogin),
			ResolverUnknown: th.IsResolved && th.ResolvedBy == "",
		}
		for j := 1; j < len(th.Comments); j++ {
			c := &th.Comments[j]
			if tm, own := OwnMarker(c, botLogin); own && tm.ID == t.ID && tm.IsTransition() {
				t.apply(tm, c)
				t.Own = append(t.Own, *c)
			}
		}
		out = append(out, t)
	}
	return out
}

func (t *Tracked) apply(m *Marker, c *github.Comment) {
	t.Last = m
	t.State = m.To
	if t.State == StateNeedsHuman {
		// Earlier versions sent disputed findings to a human and left the
		// thread open. Now a person settles a finding by resolving it.
		t.State = StateOpen
	}
	t.Fix = nil
	if m.To == StateFixed {
		t.Fix = &Record{At: c.CreatedAt, SHA: m.SHA, Files: m.Evidence}
	}
}

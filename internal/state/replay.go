package state

import "github.com/SnapdragonPartners/buggrit/internal/github"

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
	// FixSHA and Evidence come from the latest fixed transition: the head
	// the fix was verified at and the files the verdict relied on. They're
	// set only while State is fixed.
	FixSHA   string
	Evidence []string
	// Pending are the human comments no transition has handled yet, oldest
	// first.
	Pending []github.Comment
	// Rebuttals counts buggrit's rebuttal replies (§7 rule 4).
	Rebuttals int
	// Seen is the newest human comment any transition handled.
	Seen int64
	// LastID is the database ID of the comment that carries Last.
	LastID int64
}

// Blocking reports whether the finding blocks convergence (P0 or P1, §5).
func (t *Tracked) Blocking() bool { return t.Severity == "P0" || t.Severity == "P1" }

// Replay reads buggrit's findings from a PR's threads. A thread is a finding
// if its root comment carries a finding marker and is buggrit's own (§3);
// other threads belong to other reviewers. Within a thread, only buggrit's
// own transition markers for that finding count, in order. Every other
// comment is a human's, under the trust model in §1; the engine drops
// comments from outside the team before replay (§1, "Public repositories").
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
		t := Tracked{Thread: th, Root: root, ID: m.ID, FP: m.FP, Severity: m.Severity, RaisedSHA: m.SHA, State: StateOpen}
		var human []github.Comment
		for j := 1; j < len(th.Comments); j++ {
			c := &th.Comments[j]
			tm, own := OwnMarker(c, botLogin)
			switch {
			case !own:
				human = append(human, *c)
			case tm.ID == t.ID && tm.IsTransition():
				t.apply(tm, c.DatabaseID)
			}
			// buggrit's own comments with other markers are ignored.
		}
		for j := range human {
			if human[j].DatabaseID > t.Seen {
				t.Pending = append(t.Pending, human[j])
			}
		}
		out = append(out, t)
	}
	return out
}

func (t *Tracked) apply(m *Marker, id int64) {
	t.Last, t.LastID = m, id
	t.State = m.To
	t.Seen = max(t.Seen, m.Seen)
	if m.Kind == KindRebutted {
		t.Rebuttals++
	}
	t.FixSHA, t.Evidence = "", nil
	if m.To == StateFixed {
		t.FixSHA, t.Evidence = m.SHA, m.Evidence
	}
}

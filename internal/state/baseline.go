package state

import (
	"slices"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// Baseline summarizes buggrit's earlier passes on a PR, read from the
// markers in its own review bodies (design §6, "Review baseline").
type Baseline struct {
	completed map[string]bool
	// verified holds, per finding, each re-verification that found it
	// still fixed, oldest first.
	verified map[string][]Record
	// AttemptedSHA is the head of the most recent pass, complete or not.
	AttemptedSHA string
	// CompletedSHA is the head of the most recent complete pass.
	CompletedSHA string
	// LastPass is the highest pass number so far; 0 before the first pass.
	LastPass int
}

// ReadBaseline replays reviews, oldest first. Only buggrit's own reviews
// count (the same check as OwnMarker).
func ReadBaseline(reviews []github.ReviewSummary, botLogin string) Baseline {
	b := Baseline{completed: map[string]bool{}, verified: map[string][]Record{}}
	for i := range reviews {
		m, ok := ownMarker(reviews[i].Author, reviews[i].Body, botLogin)
		if !ok {
			continue
		}
		for id, files := range m.Verified {
			b.verified[id] = append(b.verified[id], Record{At: reviews[i].SubmittedAt, SHA: m.SHA, Files: files})
		}
		if !m.IsPass() {
			continue
		}
		b.LastPass = max(b.LastPass, m.Pass)
		b.AttemptedSHA = m.SHA
		if m.Status == PassComplete {
			b.CompletedSHA = m.SHA
			b.completed[m.SHA] = true
		}
	}
	return b
}

// CompletedAt reports whether some pass completed at sha.
func (b *Baseline) CompletedAt(sha string) bool { return b.completed[sha] }

// FixRecords are the candidates for a fixed finding's evidence baseline,
// newest first: its fix, and every later re-verification that found it
// still fixed. Re-verifications from before the fix belong to an earlier
// fix that has since regressed. Nil for a finding that isn't fixed.
func (b *Baseline) FixRecords(t *Tracked) []Record {
	if t.State != StateFixed || t.Fix == nil {
		return nil
	}
	out := []Record{*t.Fix}
	rs := b.verified[t.ID]
	for i := range rs {
		if rs[i].At.After(t.Fix.At) {
			out = append(out, rs[i])
		}
	}
	slices.Reverse(out)
	return out
}

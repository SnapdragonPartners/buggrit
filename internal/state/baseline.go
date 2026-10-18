package state

import "github.com/SnapdragonPartners/buggrit/internal/github"

// Baseline summarizes buggrit's earlier passes on a PR, read from the pass
// markers in its own review bodies (design §6, "Review baseline").
type Baseline struct {
	completed map[string]bool
	// unverified maps a head to the findings its latest pass left
	// unverified.
	unverified map[string][]string
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
	b := Baseline{completed: map[string]bool{}, unverified: map[string][]string{}}
	for i := range reviews {
		m, ok := ownMarker(reviews[i].Author, reviews[i].Body, botLogin)
		if !ok || !m.IsPass() {
			continue
		}
		b.LastPass = max(b.LastPass, m.Pass)
		b.AttemptedSHA = m.SHA
		b.unverified[m.SHA] = m.Unverified
		if m.Status == PassComplete {
			b.CompletedSHA = m.SHA
			b.completed[m.SHA] = true
		}
	}
	return b
}

// CompletedAt reports whether some pass completed at sha.
func (b *Baseline) CompletedAt(sha string) bool { return b.completed[sha] }

// UnverifiedAt lists the earlier findings the latest pass at sha left
// unverified.
func (b *Baseline) UnverifiedAt(sha string) []string { return b.unverified[sha] }

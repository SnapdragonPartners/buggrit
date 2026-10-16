package review

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// Candidate is a finding that survived reconcile, with its identity and
// where its thread attaches.
type Candidate struct {
	Anchor github.Anchor
	// ID is the finding's short ID ("f-7f3a2c"), derived from FP.
	ID string
	// FP is the deterministic fingerprint used for dedupe (design §7 rule 3).
	FP string
	Finding
}

// HasSuggestion reports whether the finding's suggestion can be posted: the
// anchor must still cover exactly the head lines it replaces. A range
// narrowed or moved to file level by AnchorFor would apply it to the wrong
// lines.
func (c *Candidate) HasSuggestion() bool {
	if c.Suggestion == "" || c.Anchor.FileLevel() || c.Anchor.Side == github.SideLeft {
		return false
	}
	return c.Anchor.Line == c.Line && c.Anchor.StartLine == c.StartLine
}

type reconcileInput struct {
	ws   Workspace
	diff *github.Diff
	// known holds the fingerprints of buggrit's earlier findings.
	known  map[string]bool
	review *config.Review
}

type reconciled struct {
	kept           []Candidate
	duplicates     int
	belowThreshold int
}

// reconcile applies the thresholds, anchors each finding, and drops exact
// duplicates of earlier findings and of each other (design §7 rule 3).
// Near-duplicate detection by model, and reopening a fixed finding that
// comes back, arrive with build steps 4 and 5.
func reconcile(findings []Finding, in *reconcileInput) reconciled {
	var out reconciled
	seen := map[string]bool{}
	for i := range findings {
		f := &findings[i]
		if !meetsThresholds(f, in.review) {
			out.belowThreshold++
			continue
		}
		fp := fingerprint(f, in.ws, in.diff)
		if in.known[fp] || seen[fp] {
			out.duplicates++
			continue
		}
		seen[fp] = true
		out.kept = append(out.kept, Candidate{Finding: *f, ID: "f-" + fp[:6], FP: fp, Anchor: anchor(f, in.diff)})
	}
	return out
}

// meetsThresholds applies review.min_severity and review.min_confidence.
// submit_findings has already validated both fields.
func meetsThresholds(f *Finding, r *config.Review) bool {
	sev, _ := config.SeverityRank(f.Severity)
	minSev, _ := config.SeverityRank(r.MinSeverity)
	conf, _ := config.ConfidenceRank(f.Confidence)
	minConf, _ := config.ConfidenceRank(r.MinConfidence)
	return sev <= minSev && conf >= minConf
}

// anchor maps a validated finding to a GitHub anchor (design §8).
func anchor(f *Finding, d *github.Diff) github.Anchor {
	if f.Side == string(github.SideLeft) {
		a := github.Anchor{Path: f.Path, Side: github.SideLeft, Line: f.Line}
		if fd := d.File(f.Path); fd != nil && f.StartLine > 0 && fd.CommentableRange(f.StartLine, f.Line, github.SideLeft) {
			a.StartLine = f.StartLine
		}
		return a
	}
	a, err := d.AnchorFor(f.Path, f.StartLine, f.Line)
	if err != nil {
		// submit_findings only accepts changed files.
		return github.Anchor{Path: f.Path}
	}
	return a
}

// fingerprint identifies a finding by path, category and the normalized
// text it's anchored to, so the same issue found again on a later pass,
// even at a moved line, matches (design §7 rule 3). A file-level finding has
// no anchored text and uses its normalized title instead.
func fingerprint(f *Finding, ws Workspace, d *github.Diff) string {
	snippet := strings.ToLower(normalizeSpace(f.Title))
	if f.Line > 0 {
		if text := anchoredText(f, ws, d); text != "" {
			snippet = text
		}
	}
	sum := sha256.Sum256([]byte(f.Path + "\x00" + f.Category + "\x00" + snippet))
	return hex.EncodeToString(sum[:8])
}

// anchoredText returns the finding's lines, whitespace-normalized: head
// lines from the workspace, or deleted lines from the diff for side LEFT.
func anchoredText(f *Finding, ws Workspace, d *github.Diff) string {
	first := f.Line
	if f.StartLine > 0 {
		first = f.StartLine
	}
	var lines []string
	if f.Side == string(github.SideLeft) {
		if fd := d.File(f.Path); fd != nil {
			for hi := range fd.Hunks {
				for li := range fd.Hunks[hi].Lines {
					l := &fd.Hunks[hi].Lines[li]
					if l.Kind == github.LineDeleted && l.Old >= first && l.Old <= f.Line {
						lines = append(lines, l.Text)
					}
				}
			}
		}
	} else if name, err := cleanPath(f.Path); err == nil {
		_, _ = scanLines(ws, name, func(n int, line []byte, _ int) bool {
			if n >= first {
				lines = append(lines, string(line))
			}
			return n < f.Line
		})
	}
	var kept []string
	for _, l := range lines {
		if l = normalizeSpace(l); l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// normalizeSpace collapses runs of whitespace and trims the ends.
func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

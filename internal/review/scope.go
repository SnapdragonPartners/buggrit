package review

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// Incremental is where a later pass may anchor new findings: the lines
// changed since the last completed pass (design §7 rule 2). The finder still
// sees the whole PR as context. A nil *Incremental is the whole PR.
type Incremental struct {
	// lines maps a path to the head-side lines that may anchor a finding.
	lines map[string]map[int]bool
	// files holds every path with a change in scope, including a deletion
	// that has no head-side lines.
	files map[string]bool
	// leftOK holds the files in scope whose PR diff deletes lines, so a
	// finding may anchor on a deleted line (side LEFT).
	leftOK map[string]bool
	// Since is the completed pass's head the scope starts from.
	Since string
}

// newIncremental works out the scope from the PR's diff and the diff of the
// changes since the completed pass (design §7 rule 2):
//   - A line is in scope if the changes since then add it and the PR's diff
//     adds it too, so a merge from the base branch brings nothing into scope.
//   - A run of lines only deleted since then puts its head-side neighbors in
//     scope if the PR's diff also deletes next to them. That's where a
//     finding about the deletion is anchored, and requiring the PR-side
//     deletion keeps a merge's deletions out.
//   - A change with no head-side lines to anchor on puts the file in scope
//     for file-level and deleted-line findings: a deletion that leaves no
//     lines (the file was emptied or deleted), or a change with no text
//     hunks at all (a binary file, an empty file deleted, a mode change or a
//     pure rename) when the PR's diff makes the same kind of change.
func newIncremental(since string, pr, changes *github.Diff, inScope func(string) bool) *Incremental {
	inc := &Incremental{Since: since, lines: map[string]map[int]bool{}, files: map[string]bool{}, leftOK: map[string]bool{}}
	for i := range changes.Files {
		ch := &changes.Files[i]
		p := ch.Path()
		pf := pr.File(p)
		if pf == nil || !inScope(p) {
			continue
		}
		set, fileLevel := fileScope(ch, pf)
		if len(set) > 0 {
			inc.lines[p] = set
		}
		if len(set) > 0 || fileLevel {
			inc.files[p] = true
			inc.leftOK[p] = deletesLines(pf)
		}
	}
	return inc
}

// fileScope applies newIncremental's rules to one file: ch is its change
// since the completed pass and pf its PR diff. fileLevel reports a change
// that has no head-side lines to anchor on.
func fileScope(ch, pf *github.FileDiff) (set map[int]bool, fileLevel bool) {
	added, prDeletions := map[int]bool{}, map[int]bool{}
	for _, n := range pf.AddedLines() {
		added[n] = true
	}
	for h := range pf.Hunks {
		for _, n := range deletionNeighbors(&pf.Hunks[h], false) {
			prDeletions[n] = true
		}
	}
	set = map[int]bool{}
	for h := range ch.Hunks {
		hunk := &ch.Hunks[h]
		for li := range hunk.Lines {
			if l := &hunk.Lines[li]; l.Kind == github.LineAdded && added[l.New] {
				set[l.New] = true
			}
		}
		for _, n := range deletionNeighbors(hunk, true) {
			if prDeletions[n] {
				set[n] = true
			}
		}
		fileLevel = fileLevel || hunk.NewLines == 0 && len(hunk.Lines) > 0
	}
	return set, fileLevel || len(ch.Hunks) == 0 && samePRChange(ch, pf)
}

// samePRChange reports whether the PR's diff makes the same kind of
// hunkless change as ch: a deletion, an added empty file, a rename, a binary
// change or a mode change. A merge from the base branch can bring in such a
// change on a file the PR touched for other reasons; this keeps it out.
func samePRChange(ch, pf *github.FileDiff) bool {
	switch {
	case ch.Status == github.StatusDeleted && pf.Status == github.StatusDeleted,
		ch.Status == github.StatusAdded && pf.Status == github.StatusAdded,
		ch.Status == github.StatusRenamed && pf.Status == github.StatusRenamed,
		ch.Binary && pf.Binary,
		ch.ModeChanged && pf.ModeChanged:
		return true
	}
	return false
}

// deletesLines reports whether a file's diff deletes any lines.
func deletesLines(f *github.FileDiff) bool {
	for h := range f.Hunks {
		for li := range f.Hunks[h].Lines {
			if f.Hunks[h].Lines[li].Kind == github.LineDeleted {
				return true
			}
		}
	}
	return false
}

// deletionNeighbors returns the head-side lines next to each run of deleted
// lines in a hunk. With pureOnly, a run next to an added line is skipped: it
// is a replacement, which the added lines already cover.
func deletionNeighbors(h *github.Hunk, pureOnly bool) []int {
	var out []int
	prev, prevAdded, pending := 0, false, false
	for i := range h.Lines {
		l := &h.Lines[i]
		if l.Kind == github.LineDeleted {
			pending = true
			continue
		}
		added := l.Kind == github.LineAdded
		if pending && (!pureOnly || !added && !prevAdded) {
			if prev > 0 {
				out = append(out, prev)
			}
			out = append(out, l.New)
		}
		pending = false
		prev, prevAdded = l.New, added
	}
	if pending && (!pureOnly || !prevAdded) && prev > 0 {
		out = append(out, prev)
	}
	return out
}

// Empty reports whether nothing reviewable changed since the completed pass.
func (inc *Incremental) Empty() bool { return inc != nil && len(inc.files) == 0 }

// paths returns the files in scope, sorted.
func (inc *Incremental) paths() []string {
	out := make([]string, 0, len(inc.files))
	for p := range inc.files {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// pathList renders the files in scope, comma-separated, within about limit
// bytes.
func (inc *Incremental) pathList(limit int) string {
	paths := inc.paths()
	var b strings.Builder
	for i, p := range paths {
		if b.Len() > limit {
			fmt.Fprintf(&b, ", and %d more files", len(paths)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p)
	}
	return b.String()
}

// maxRanges bounds one file's rendered ranges, in the prompt and in a
// submit_findings error, and the file list in that error.
const maxRanges = 4 << 10

// The scope of a file with no head-side lines in scope. A deleted line is
// offered only where the PR's diff has one, since side LEFT needs it.
const (
	fileLevelOnly = "no head-side lines; anchor at file level (line 0)"
	fileOrDeleted = "no head-side lines; anchor at file level (line 0) or on a deleted line (side LEFT)"
)

// ranges renders a file's lines in scope as "12-20, 44", within maxRanges.
func (inc *Incremental) ranges(p string) string {
	set := inc.lines[p]
	if len(set) == 0 {
		if inc.leftOK[p] {
			return fileOrDeleted
		}
		return fileLevelOnly
	}
	lines := make([]int, 0, len(set))
	for n := range set {
		lines = append(lines, n)
	}
	slices.Sort(lines)
	var b strings.Builder
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		if b.Len() > maxRanges {
			fmt.Fprintf(&b, ", and %d more lines from %d on", len(lines)-i, lines[i])
			break
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		if i == j {
			b.WriteString(strconv.Itoa(lines[i]))
		} else {
			fmt.Fprintf(&b, "%d-%d", lines[i], lines[j])
		}
		i = j + 1
	}
	return b.String()
}

// check reports why f's published anchor (a) is outside the scope, or "".
// Only the anchor is checked: a class finding's other instances may be
// anywhere (§7 rule 1). A file-level finding, or one on a deleted line,
// needs only its file to have changed. A finding on head lines must be
// published on a line in scope: a range is checked as GitHub will get it,
// after AnchorFor narrows it, and a line that would fall back to file level
// is out of scope.
func (inc *Incremental) check(f *Finding, a github.Anchor) string {
	if inc == nil {
		return ""
	}
	if !inc.files[f.Path] {
		return fmt.Sprintf("%s has no changes since %s, the last completed review. This pass reports new findings only "+
			"on lines changed since then, in: %s", f.Path, short(inc.Since), inc.pathList(maxRanges))
	}
	if f.Line == 0 || a.Side == github.SideLeft {
		return ""
	}
	start := a.StartLine
	if start == 0 {
		start = a.Line
	}
	for n := start; n <= a.Line && !a.FileLevel(); n++ {
		if inc.lines[f.Path][n] {
			return ""
		}
	}
	return fmt.Sprintf("line %d of %s hasn't changed since %s, the last completed review (a range is anchored on its "+
		"last line unless it lies within one diff hunk). This pass reports new findings only on lines changed since then; "+
		"in %s those are %s", f.Line, f.Path, short(inc.Since), f.Path, inc.ranges(f.Path))
}

// passScope picks the pass's scope (design §6 "Review baseline", §7 rule 2):
// the whole PR when no pass has completed, or when the completed pass's head
// isn't an ancestor of HEAD (a force-push or rebase); otherwise the lines
// changed since it. It also returns a description for the review body.
func passScope(ctx context.Context, gh GitHub, ref PRRef, since string, pr *github.Diff, inScope func(string) bool) (*Incremental, string, error) {
	const whole = "the whole pull request"
	if since == "" {
		return nil, whole, nil
	}
	status, err := gh.CompareStatus(ctx, ref.Repo, since, ref.HeadSHA)
	switch {
	case github.IsNotFound(err):
		return nil, fmt.Sprintf("%s, since GitHub no longer has %s, the last completed review's head", whole, short(since)), nil
	case err != nil:
		return nil, "", fmt.Errorf("scope: %w", err)
	case status != github.CompareAhead && status != github.CompareIdentical:
		return nil, fmt.Sprintf("%s, since %s, the last completed review's head, is no longer in its history", whole, short(since)), nil
	}
	changes, err := gh.CompareDiff(ctx, ref.Repo, since, ref.HeadSHA)
	switch {
	case errors.Is(err, github.ErrDiffTooLarge):
		return nil, fmt.Sprintf("%s, since the changes after %s are too large to diff", whole, short(since)), nil
	case err != nil:
		return nil, "", fmt.Errorf("scope: %w", err)
	}
	return newIncremental(since, pr, changes, inScope), "lines changed since " + short(since), nil
}

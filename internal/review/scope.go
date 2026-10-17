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
	// Since is the completed pass's head the scope starts from.
	Since string
}

// newIncremental works out the scope from the PR's diff and the diff of the
// changes since the completed pass. A line is in scope if the changes since
// then add it and the PR's diff adds it too, so a merge from the base branch
// brings nothing into scope. A deletion since then puts its neighboring
// head-side lines in scope when the PR's diff shows them, since that's where
// a finding about the deletion is anchored.
func newIncremental(since string, pr, changes *github.Diff, inScope func(string) bool) *Incremental {
	inc := &Incremental{Since: since, lines: map[string]map[int]bool{}, files: map[string]bool{}}
	for i := range changes.Files {
		ch := &changes.Files[i]
		p := ch.Path()
		pf := pr.File(p)
		if pf == nil || !inScope(p) {
			continue
		}
		if ch.Status == github.StatusDeleted {
			if pf.Status == github.StatusDeleted {
				inc.files[p] = true
			}
			continue
		}
		added := map[int]bool{}
		for _, n := range pf.AddedLines() {
			added[n] = true
		}
		set := map[int]bool{}
		for h := range ch.Hunks {
			adds, neighbors := changedLines(&ch.Hunks[h])
			for _, n := range adds {
				if added[n] {
					set[n] = true
				}
			}
			for _, n := range neighbors {
				if pf.Commentable(n, github.SideRight) {
					set[n] = true
				}
			}
		}
		if len(set) > 0 {
			inc.lines[p] = set
			inc.files[p] = true
		}
	}
	return inc
}

// changedLines returns the head-side lines a hunk adds, and the head-side
// neighbors of each run of lines it only deletes. A deletion next to added
// lines is a replacement, which the added lines already cover.
func changedLines(h *github.Hunk) (adds, neighbors []int) {
	prev, prevAdded, pending := 0, false, false
	for i := range h.Lines {
		l := &h.Lines[i]
		if l.Kind == github.LineDeleted {
			pending = true
			continue
		}
		added := l.Kind == github.LineAdded
		if added {
			adds = append(adds, l.New)
		}
		if pending && !added && !prevAdded {
			if prev > 0 {
				neighbors = append(neighbors, prev)
			}
			neighbors = append(neighbors, l.New)
		}
		pending = false
		prev, prevAdded = l.New, added
	}
	if pending && !prevAdded && prev > 0 {
		neighbors = append(neighbors, prev)
	}
	return adds, neighbors
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

// ranges renders a file's lines in scope as "12-20, 44", or "the deletion"
// for a file deleted since the completed pass.
func (inc *Incremental) ranges(p string) string {
	set := inc.lines[p]
	if len(set) == 0 {
		return "the deletion"
	}
	lines := make([]int, 0, len(set))
	for n := range set {
		lines = append(lines, n)
	}
	slices.Sort(lines)
	var parts []string
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(lines[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", lines[i], lines[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// check reports why f's anchor is outside the scope, or "". Only the anchor
// is checked: a class finding's other instances may be anywhere (§7 rule 1).
// A file-level finding, or one on a deleted line, needs only its file to
// have changed.
func (inc *Incremental) check(f *Finding) string {
	if inc == nil {
		return ""
	}
	if !inc.files[f.Path] {
		return fmt.Sprintf("%s has no changes since %s, the last completed review. This pass reports new findings only "+
			"on lines changed since then, in: %s", f.Path, short(inc.Since), strings.Join(inc.paths(), ", "))
	}
	if f.Line == 0 || github.Side(f.Side) == github.SideLeft {
		return ""
	}
	start := f.StartLine
	if start == 0 {
		start = f.Line
	}
	for n := start; n <= f.Line; n++ {
		if inc.lines[f.Path][n] {
			return ""
		}
	}
	return fmt.Sprintf("line %d of %s hasn't changed since %s, the last completed review. This pass reports new findings "+
		"only on lines changed since then; in %s those are %s", f.Line, f.Path, short(inc.Since), f.Path, inc.ranges(f.Path))
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

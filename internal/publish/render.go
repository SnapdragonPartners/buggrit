// Package publish renders a pass's findings as review comments and posts
// them, fenced to the reviewed head (docs/design.md §2, §8).
package publish

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/SnapdragonPartners/buggrit/internal/review"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// FindingBody renders one finding's thread comment (design §8), ending with
// its marker. pass and sha identify the pass that raised it.
func FindingBody(c *review.Candidate, pass int, sha string) (string, error) {
	text := findingText(c, c.HasSuggestion())
	if len(text) > maxBody && c.HasSuggestion() {
		// Truncating would cut through the suggestion block and leave
		// broken replacement code, so it goes as a whole.
		text = findingText(c, false)
	}
	body, err := state.Append(capBody(text), &state.Marker{ID: c.ID, FP: c.FP, Severity: c.Severity, Pass: pass, SHA: sha})
	if err != nil {
		return "", fmt.Errorf("render %s: %w", c.ID, err)
	}
	return body, nil
}

// findingText renders a finding's comment without its marker.
func findingText(c *review.Candidate, suggestion bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s · %s** · confidence: %s — %s\n\n", c.Severity, c.Category, c.Confidence, c.Title)
	b.WriteString(strings.TrimSpace(c.Problem))
	b.WriteString("\n\n")
	if impact := strings.TrimSpace(c.Impact); impact != "" {
		fmt.Fprintf(&b, "**Impact:** %s\n\n", impact)
	}
	fmt.Fprintf(&b, "**Fix:** %s\n\n", strings.TrimSpace(c.Fix))
	if suggestion {
		fence := codeFence(c.Suggestion)
		fmt.Fprintf(&b, "%ssuggestion\n%s\n%s\n\n", fence, strings.TrimSuffix(c.Suggestion, "\n"), fence)
	}
	if len(c.Instances) > 0 {
		locs := make([]string, len(c.Instances))
		for i, l := range c.Instances {
			locs[i] = "`" + l.Path + "`"
			if l.Line > 0 {
				locs[i] = fmt.Sprintf("`%s:%d`", l.Path, l.Line)
			}
		}
		fmt.Fprintf(&b, "Also applies to: %s (same class: *%s*).\n\n", strings.Join(locs, ", "), c.Class)
	}
	weight := "advisory, non-blocking"
	if c.Blocking() {
		weight = "blocking"
	}
	fmt.Fprintf(&b, "<sub>buggrit %s · %s · Fix it, or reply explaining why not.</sub>", c.ID, weight)
	return b.String()
}

// codeFence returns a backtick fence longer than any run of backticks in s,
// so a suggestion containing a code block can't close it early.
func codeFence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// Status is the pass's one-line outcome, for the review body and the job
// summary.
func Status(res *review.Result) string {
	at := short(res.HeadSHA)
	switch {
	case res.Skipped:
		return fmt.Sprintf("skipped at %s: %s", at, res.SkipReason)
	case !res.Complete:
		return fmt.Sprintf("incomplete at %s: %s", at, res.Reason)
	case len(res.Findings) == 0:
		return fmt.Sprintf("complete at %s: no new findings", at)
	}
	blocking := 0
	for i := range res.Findings {
		if res.Findings[i].Blocking() {
			blocking++
		}
	}
	return fmt.Sprintf("complete at %s: %d new %s (%d blocking, %d advisory)",
		at, len(res.Findings), plural(len(res.Findings), "finding"), blocking, len(res.Findings)-blocking)
}

// ReviewBody renders the pass's review body, ending with its pass marker
// (design §6, "Review baseline").
func ReviewBody(res *review.Result) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "**buggrit** pass %d\n\n", res.Pass)
	if s := strings.TrimSpace(res.Summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "**Status:** %s.", Status(res))
	switch {
	case !res.Complete && len(res.Findings) == 0:
		b.WriteString(" Nothing from this pass was posted; the next run at this commit tries again.")
	case !res.Complete:
		b.WriteString(" The findings it reached are posted; the next run at this commit reviews it again.")
	}
	b.WriteString("\n")
	var dropped []string
	if res.Duplicates > 0 {
		dropped = append(dropped, fmt.Sprintf("%d already raised", res.Duplicates))
	}
	if res.BelowThreshold > 0 {
		dropped = append(dropped, fmt.Sprintf("%d below the configured thresholds", res.BelowThreshold))
	}
	if len(dropped) > 0 {
		fmt.Fprintf(&b, "\nNot posted: %s.\n", strings.Join(dropped, ", "))
	}
	status := state.PassComplete
	if !res.Complete {
		status = state.PassIncomplete
	}
	body, err := state.Append(capBody(b.String()), &state.Marker{ID: fmt.Sprintf("pass-%d", res.Pass), Pass: res.Pass, SHA: res.HeadSHA, Status: status})
	if err != nil {
		return "", fmt.Errorf("render review body: %w", err)
	}
	return body, nil
}

// maxBody keeps a comment under GitHub's 65,536-character limit with room
// for the marker.
const maxBody = 60000

// capBody shortens an over-long body (a model that wrote an essay) rather
// than letting GitHub reject the whole review.
func capBody(s string) string {
	if len(s) <= maxBody {
		return s
	}
	n := maxBody
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n\n… [truncated]"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

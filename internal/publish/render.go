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
	fmt.Fprintf(&b, "<sub>buggrit %s · %s · %s</sub>", c.ID, weight, actNote)
	return b.String()
}

// actNote tells the reader of a finding how buggrit treats what they do
// next (design §8): it checks the code on every push, and resolving the
// thread settles the finding.
const actNote = "To fix it, push the fix and leave this thread open: buggrit checks on the next push and resolves it. " +
	"Not fixing it? Resolve the thread; that settles it, and buggrit won't raise it again."

// TransitionBody renders the reply that records a transition of an earlier
// finding, at head sha, ending with its transition marker (design §6).
// buggrit posts only two kinds: fixed and regressed.
func TransitionBody(tr *review.Transition, sha string) (string, error) {
	t := tr.Tracked
	at := "`" + short(sha) + "`"
	reason := strings.TrimSpace(tr.Reason)
	var lead, note string
	switch tr.Kind {
	case state.KindFixed:
		lead = "**Fixed** at " + at + ". " + reason
		note = "checked against " + strings.Join(tr.Evidence, ", ") + "; if those files change, buggrit checks again"
	case state.KindRegressed:
		lead = "**Regressed** at " + at + ", so this thread is reopened. " + reason
		note = actNote
	default:
		return "", fmt.Errorf("render %s: buggrit doesn't post %q transitions", t.ID, tr.Kind)
	}
	text := fmt.Sprintf("%s\n\n<sub>buggrit %s · %s</sub>", strings.TrimSpace(lead), t.ID, note)
	m := &state.Marker{ID: t.ID, To: tr.Kind.To(), Kind: tr.Kind, SHA: sha, Evidence: tr.Evidence}
	body, err := state.Append(capBody(text), m)
	if err != nil {
		return "", fmt.Errorf("render %s: %w", t.ID, err)
	}
	return body, nil
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
	case res.Sync:
		e := earlier(res)
		if e == "" {
			e = "nothing to repair"
		}
		return fmt.Sprintf("synced at %s: %s", at, e)
	}
	var parts []string
	if !res.ReconcileOnly && res.Complete {
		parts = append(parts, newFindings(res))
	}
	if e := earlier(res); e != "" {
		parts = append(parts, e)
	}
	if !res.Done() {
		return fmt.Sprintf("incomplete at %s: %s", at, strings.Join(append([]string{res.Reason}, parts...), "; "))
	}
	return fmt.Sprintf("complete at %s: %s", at, strings.Join(parts, "; "))
}

func newFindings(res *review.Result) string {
	if len(res.Findings) == 0 {
		return "no new findings"
	}
	blocking := 0
	for i := range res.Findings {
		if res.Findings[i].Blocking() {
			blocking++
		}
	}
	return fmt.Sprintf("%d new %s (%d blocking, %d advisory)",
		len(res.Findings), plural(len(res.Findings), "finding"), blocking, len(res.Findings)-blocking)
}

// kindLabel names a kind of transition in the status line; a step with no
// kind is a repair.
func kindLabel(k state.Kind) string {
	if k == "" {
		return "repaired"
	}
	return string(k)
}

// earlier summarizes what the pass did with earlier findings, or "" if it
// did nothing visible. A finding verified with no change counts as
// unchanged.
func earlier(res *review.Result) string {
	counts := map[string]int{}
	var order []string
	add := func(label string) {
		if counts[label] == 0 {
			order = append(order, label)
		}
		counts[label]++
	}
	changed := map[string]bool{}
	for i := range res.Transitions {
		tr := &res.Transitions[i]
		add(kindLabel(tr.Kind))
		changed[tr.Tracked.ID] = tr.Kind != ""
	}
	for i := range res.Checks {
		if id := res.Checks[i].Tracked.ID; res.Verify != nil && res.Verify.Verdicts[id] != nil && !changed[id] {
			add("unchanged")
		}
	}
	if len(order) == 0 {
		return ""
	}
	out := make([]string, len(order))
	for i, label := range order {
		out[i] = fmt.Sprintf("%d %s", counts[label], label)
	}
	return "earlier findings: " + strings.Join(out, ", ")
}

// ReviewBody renders the pass's review body, ending with its pass marker
// (design §6, "Review baseline"), which also records the fixed findings
// the pass re-verified. manual lists threads GitHub wouldn't resolve or
// reopen for buggrit (§3).
func ReviewBody(res *review.Result, manual []Manual) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "**buggrit** pass %d\n\n", res.Pass)
	if res.Scope != "" {
		fmt.Fprintf(&b, "**Scope:** %s.\n\n", res.Scope)
	}
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
	case len(res.Unverified) > 0:
		b.WriteString(" Earlier findings it didn't reach are checked on the next run.")
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
	writeLists(&b, res, manual)
	status := state.PassComplete
	if !res.Complete {
		status = state.PassIncomplete
	}
	m := &state.Marker{ID: fmt.Sprintf("pass-%d", res.Pass), Pass: res.Pass, SHA: res.HeadSHA, Status: status, Verified: res.Verified}
	body, err := state.Append(capBody(b.String()), m)
	if err != nil {
		return "", fmt.Errorf("render review body: %w", err)
	}
	return body, nil
}

// writeLists adds the threads a person has to look at.
func writeLists(b *strings.Builder, res *review.Result, manual []Manual) {
	if len(manual) > 0 {
		b.WriteByte('\n')
		b.WriteString(ManualList(manual))
	}
	if len(res.UnknownResolver) > 0 {
		b.WriteByte('\n')
		b.WriteString(UnknownList(res))
	}
}

// ManualList asks a person to make the resolution changes GitHub refused
// buggrit (§3).
func ManualList(manual []Manual) string {
	var b strings.Builder
	b.WriteString("GitHub didn't let buggrit change these threads' resolution; please do it by hand:\n")
	for i := range manual {
		verb := "reopen"
		if manual[i].Resolve {
			verb = "resolve"
		}
		fmt.Fprintf(&b, "- %s %s %s\n", verb, manual[i].ID, manual[i].URL)
	}
	return b.String()
}

// UnknownList asks a person about open findings whose threads are resolved
// by an account GitHub no longer reports, so buggrit can't tell whether
// they're settled (§6).
func UnknownList(res *review.Result) string {
	var b strings.Builder
	b.WriteString("These open findings' threads are resolved, but GitHub doesn't say by whom, so buggrit can't tell whether they're settled. " +
		"Resolve each again to settle it, or reopen it to keep it open:\n")
	for _, t := range res.UnknownResolver {
		fmt.Fprintf(&b, "- %s %s\n", t.ID, t.Root.URL)
	}
	return b.String()
}

// ReconcileBody is the review a run that posts no pass review posts when it
// has something to record (§3, §6): the fixed findings it re-verified, in
// its marker, and the resolutions GitHub refused it. The marker carries no
// pass status, so the baseline doesn't count it as a pass.
func ReconcileBody(res *review.Result, manual []Manual) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "**buggrit** at %s\n", short(res.HeadSHA))
	if n := len(res.Verified); n > 0 {
		fmt.Fprintf(&b, "\nRe-checked %d fixed %s whose files changed; still fixed.\n", n, plural(n, "finding"))
	}
	writeLists(&b, res, manual)
	body, err := state.Append(capBody(b.String()), &state.Marker{ID: "reconcile", SHA: res.HeadSHA, Verified: res.Verified})
	if err != nil {
		return "", fmt.Errorf("render reconcile review: %w", err)
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

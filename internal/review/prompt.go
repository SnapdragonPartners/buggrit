package review

import (
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// Prompt budgets. Past these, the opening message lists what it left out and
// the model reads the rest with tools. Splitting large PRs into sub-reviews
// is build step 6 (design §12).
//
// maxPromptDiffBytes bounds the whole opening message: diffs are added only
// while it fits. The sections before the diff have budgets of their own that
// sum to well under it, so they can't crowd the message past it.
const (
	maxPromptDiffBytes    = 300 << 10
	maxDescriptionBytes   = 32 << 10
	maxInstructionBytes   = 50 << 10  // one instruction file
	maxInstructionsTotal  = 100 << 10 // every instruction file, plus review.instructions
	maxOtherThreads       = 50
	maxOtherThreadExcerpt = 500
	maxPromptFileList     = 48 << 10
	// maxDeferredList is held back from the diff for the closing list of
	// files deferred to get_diff and the final instruction. The verifier
	// holds it back from its findings in the same way, for the list of
	// findings deferred to get_thread (bounded by maxVerifyClosing).
	maxDeferredList = 16 << 10
)

// systemPrompt is the finder's standing instructions (design §5, §7).
const systemPrompt = `You are buggrit, a code reviewer for a pull request. You report real defects so the author can fix them before merge. Another agent, usually Claude Code, reads each finding as its own review thread and acts on it.

How to work:
- The opening message has the pull request description, the repository's instructions, and the diff with head-side line numbers. Start from it, then investigate with the tools: read_file, grep, list_dir, get_diff. Read callers, tests, docs and design files the change touches or relies on. All tools are read-only.
- Batch your tool calls. The tools return almost instantly; nearly all of a review's time is your own thinking between calls, so every turn costs time whether it makes one call or six. Whenever you know what you need next, request it all in one turn: read a file together with its callers and tests, run several greps at once. Fewer, fuller turns leave more of your time budget for reasoning about what you found.
- When you are done, call submit_findings exactly once with every finding. If there is nothing worth raising, submit an empty list. Do not answer in plain text.
- You have a time budget, stated in the opening message, and every tool result ends with the time used. Plan to submit before the wrap-up time. Set coverage to "complete" only if you reviewed everything in this pass's scope: the whole change on a first pass, or the lines listed under "Scope of this pass" on a later one. Otherwise set "partial" and say in summary what you did not cover. A partial review is reviewed again on the next run, so report honestly rather than rushing.

What to report:
- Concrete problems introduced or exposed by this change: bugs, unsafe concurrency, mishandled errors, security exposures, broken API contracts, missing tests for new behavior, and documentation drift. Documentation drift means docs, ADRs, help text, comments or config examples that this change made inaccurate; look for it actively, including in files the diff did not touch.
- Not: style preferences the repository's instructions don't ask for, speculative concerns you could not confirm by reading the code, or things the pull request description says it intentionally does not do.
- Respect the repository's instructions and design documents. A deliberate, documented decision is not a defect.

Severity:
- P0: data loss, corruption, critical security exposure, or an unusable result.
- P1: a concrete correctness, robustness, architectural or maintainability defect.
- P2: a real issue that doesn't block merge, such as documentation drift, a missing edge-case test, or a misleading name or comment.
- P3: a minor nit.
Confidence is how sure you are after reading the code: high (verified), medium (likely), low (plausible but unconfirmed). Prefer to investigate until you are sure.

Report each pattern once, completely:
- When you find an issue, search for every other instance of the same pattern in the change and the code it touches. Report them as one finding: anchor it at the first instance, set class to a short name for the pattern, and list every other instance in instances. Frame the fix as one change that handles every case. Never report one instance and leave the rest for a later round.
- Don't repeat issues already raised in the existing review threads listed in the opening message.

Anchoring:
- path must be a file this pull request changes. On a later pass, the opening message's "Scope of this pass" section narrows where findings may be anchored; follow its rules. line is a head-side line number as shown in the diff or by read_file; use start_line for a range. A finding about a file the diff doesn't touch (for example a stale doc) is anchored at the changed line that made it wrong, and names the other file in problem and instances.
- suggestion, if given, is the exact replacement text for the anchored lines and nothing else.

Writing: be specific and brief. problem says what is wrong and why, citing code. impact says what breaks. fix says what to change.`

// Context is what the finder knows about the pull request.
type Context struct {
	Title   string
	Body    string
	HeadSHA string
	BaseRef string
	Number  int
}

// ThreadNote is an existing review thread, from another reviewer or from an
// earlier buggrit pass, given to the finder so it doesn't repeat it (design
// §6).
type ThreadNote struct {
	Path     string
	Author   string
	Body     string
	Line     int
	Resolved bool
}

// packing is what an opening message carried and what it left to the
// tools, for telemetry: the first experiments tune it (docs/telemetry.md).
// Sizes are in bytes.
type packing struct {
	Instructions        []packedFile `json:"instructions"`
	InstructionsOmitted []string     `json:"instructions_omitted"`
	Diff                []packedFile `json:"diff"`
	// Deferred are the files, or for the verifier the findings, left to
	// get_diff or get_thread.
	Deferred []string `json:"deferred"`
	// Checks are the findings the verifier's opening shows in full.
	Checks               []string `json:"checks,omitempty"`
	Bytes                int      `json:"bytes"`
	DescriptionBytes     int      `json:"description_bytes"`
	ReviewInstructions   int      `json:"review_instructions_bytes"`
	OtherThreads         int      `json:"other_threads"`
	OtherThreadsShown    int      `json:"other_threads_shown"`
	DescriptionTruncated bool     `json:"description_truncated"`
	ReviewTruncated      bool     `json:"review_instructions_truncated"`
}

type packedFile struct {
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}

// openingMessage builds the finder's first user message, and records in pk
// what it carried.
func openingMessage(in *FindInput, pk *packing) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Pull request #%d: %s\n\n", in.PR.Number, in.PR.Title)
	fmt.Fprintf(&b, "Base branch: %s. Reviewed head commit: %s.\n\n", in.PR.BaseRef, in.PR.HeadSHA)
	writeDescription(&b, in.PR.Body, pk)
	writeInstructions(&b, in.Workspace, in.Instructions, in.InstructionFiles, pk)
	writeOtherThreads(&b, in.OtherThreads, pk)

	b.WriteString("## Changed files\n\n")
	b.WriteString(fileList(in.Diff, in.Scope.InScope, 0, maxPromptFileList))
	b.WriteString("\nFiles marked \"excluded from review\" may be read for context but must not be the subject of findings.\n\n")

	writeIncremental(&b, in.Incremental)

	b.WriteString("## Diff\n\nHead-side line numbers are on the left; deleted lines have none.\n\n")
	var skipped []string
	for _, f := range diffOrder(in) {
		text := renderFileDiff(f, 0, maxResultBytes)
		if b.Len()+len(text) > maxPromptDiffBytes-maxDeferredList {
			skipped = append(skipped, f.Path())
			continue
		}
		pk.Diff = append(pk.Diff, packedFile{Path: f.Path(), Bytes: len(text), Truncated: strings.Contains(text, "\n"+diffTruncated)})
		b.WriteString(text)
		b.WriteByte('\n')
	}
	if len(skipped) > 0 {
		b.WriteString("The diff is too large to show in full. Call get_diff for each of these files:\n")
		for i, p := range skipped {
			if b.Len() > maxPromptDiffBytes-maxDeferredList {
				fmt.Fprintf(&b, "(and %d more; page through the full list with get_diff and no path)\n", len(skipped)-i)
				break
			}
			b.WriteString(p)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	b.WriteString("Investigate as needed, then call submit_findings once with every finding.\n")
	pk.Deferred, pk.Bytes = skipped, b.Len()
	return b.String()
}

// writeDescription adds the PR description, within maxDescriptionBytes.
func writeDescription(b *strings.Builder, body string, pk *packing) {
	b.WriteString("## Description\n\n")
	if body = strings.TrimSpace(body); body != "" {
		text := truncate(body, maxDescriptionBytes)
		pk.DescriptionBytes, pk.DescriptionTruncated = len(text), len(text) < len(body)
		b.WriteString(text)
	} else {
		b.WriteString("(no description)")
	}
	b.WriteString("\n\n")
}

// diffOrder returns the in-scope files to show in the diff. On an
// incremental pass, files with changes since the completed pass come first,
// so they're the last to be deferred to get_diff.
func diffOrder(in *FindInput) []*github.FileDiff {
	var first, rest []*github.FileDiff
	for i := range in.Diff.Files {
		f := &in.Diff.Files[i]
		switch {
		case !in.Scope.InScope(f.Path()):
		case in.Incremental != nil && in.Incremental.files[f.Path()]:
			first = append(first, f)
		default:
			rest = append(rest, f)
		}
	}
	return append(first, rest...)
}

// writeIncremental tells the finder where this pass looks for new findings
// (design §7 rule 2). submit_findings enforces it, and its error names the
// lines, so a list cut short for space is recoverable.
func writeIncremental(b *strings.Builder, inc *Incremental) {
	if inc == nil {
		return
	}
	fmt.Fprintf(b, "## Scope of this pass\n\n"+
		"An earlier pass reviewed this pull request through commit %s. Report new findings only on what changed since then, "+
		"listed below. Use the rest of the pull request and the repository as context: a change here can break code elsewhere, "+
		"and a finding about that is anchored on the change.\n\n%s\nCoverage refers to what is listed here.\n\n", short(inc.Since), scopeRules)
	start := b.Len()
	paths := inc.paths()
	for i, p := range paths {
		if b.Len()-start > maxPromptFileList {
			fmt.Fprintf(b, "(and %d more files; submit_findings names the lines for a file it rejects)\n", len(paths)-i)
			break
		}
		fmt.Fprintf(b, "- %s: %s\n", p, inc.ranges(p))
	}
	b.WriteByte('\n')
}

// writeInstructions adds the repository's instruction files and
// review.instructions, within maxInstructionsTotal. review.instructions comes
// first, since the repository wrote it for buggrit. Files that don't fit are
// named, so the model can read them with read_file.
func writeInstructions(b *strings.Builder, ws Workspace, instructions string, instructionFiles []string, pk *packing) {
	budget := maxInstructionsTotal
	trimmed := strings.TrimSpace(instructions)
	extra := truncate(trimmed, maxInstructionBytes)
	budget -= len(extra)
	pk.ReviewInstructions, pk.ReviewTruncated = len(extra), len(extra) < len(trimmed)

	files := make([]string, 0, len(instructionFiles))
	var omitted []string
	seen := map[string]bool{}
	for _, name := range instructionFiles {
		p, label, ok := instructionPath(ws, name)
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		text, complete, err := readPrefix(ws, p, min(budget, maxInstructionBytes))
		switch {
		case err != nil:
			continue // missing, binary or unreadable: not an instruction file
		case text == "" && !complete:
			omitted = append(omitted, p)
			continue
		case !complete:
			text += "\n[truncated; read_file " + p + " for the rest]"
		}
		pk.Instructions = append(pk.Instructions, packedFile{Path: p, Bytes: len(text), Truncated: !complete})
		budget -= len(text)
		files = append(files, fmt.Sprintf("### %s\n\n%s\n", label, strings.TrimSpace(text)))
	}
	pk.InstructionsOmitted = omitted
	if len(files) == 0 && len(omitted) == 0 && extra == "" {
		return
	}
	b.WriteString("## Repository instructions\n\n")
	for _, f := range files {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	if len(omitted) > 0 {
		fmt.Fprintf(b, "Not shown, for space; read them with read_file: %s\n\n", strings.Join(omitted, ", "))
	}
	if extra != "" {
		b.WriteString("### Review instructions\n\n")
		b.WriteString(extra)
		b.WriteString("\n\n")
	}
}

// instructionPath resolves a configured instruction file to the path to
// read and the label to show. A symlink is followed one hop, because
// AGENTS.md -> CLAUDE.md is common; its target must pass cleanPath like any
// other path (design §10). ok is false for a missing or refused file.
func instructionPath(ws Workspace, name string) (p, label string, ok bool) {
	p, err := cleanPath(name)
	if err != nil {
		return "", "", false
	}
	info, err := ws.Lstat(p)
	if err != nil {
		return "", "", false
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return p, p, true
	}
	target, err := ws.ReadLink(p)
	if err != nil || path.IsAbs(target) {
		return "", "", false
	}
	resolved, err := cleanPath(path.Join(path.Dir(p), target))
	if err != nil {
		return "", "", false
	}
	return resolved, p + " → " + resolved, true
}

// readPrefix returns as many whole lines of a file as fit in limit bytes,
// and whether that was the whole file.
func readPrefix(ws Workspace, name string, limit int) (text string, complete bool, err error) {
	var sb strings.Builder
	complete = true
	_, err = scanLines(ws, name, func(_ int, raw []byte, size int) bool {
		line := clipSized(string(raw), size)
		if sb.Len()+len(line)+1 > limit {
			complete = false
			return false
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
		return true
	})
	return sb.String(), complete, err
}

func writeOtherThreads(b *strings.Builder, threads []ThreadNote, pk *packing) {
	pk.OtherThreads, pk.OtherThreadsShown = len(threads), min(len(threads), maxOtherThreads)
	if len(threads) == 0 {
		return
	}
	b.WriteString("## Existing review threads\n\nThese are already raised, by other reviewers or by buggrit on an earlier pass. Don't raise them again.\n\n")
	for i := range threads {
		t := &threads[i]
		if i >= maxOtherThreads {
			fmt.Fprintf(b, "(%d more not shown)\n", len(threads)-i)
			break
		}
		state := "open"
		if t.Resolved {
			state = "resolved"
		}
		loc := t.Path
		if t.Line > 0 {
			loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
		}
		body := truncate(strings.Join(strings.Fields(t.Body), " "), maxOtherThreadExcerpt)
		fmt.Fprintf(b, "- %s (%s, %s): %s\n", loc, t.Author, state, body)
	}
	b.WriteByte('\n')
}

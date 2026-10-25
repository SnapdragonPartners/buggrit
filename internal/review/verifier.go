package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// Check is an earlier finding the verifier must judge, and why it was
// selected (design §6, "What gets verified each pass").
type Check struct {
	Tracked *state.Tracked
	// Why says why it's checked, for the model.
	Why string
}

// VerifyInput is everything one verifier run needs.
type VerifyInput struct {
	Client    llms.ChatClient
	Workspace Workspace
	Diff      *github.Diff
	// OnToolCall, if set, observes each tool call.
	OnToolCall       func(toolloop.ToolCallEvent)
	PR               Context
	Instructions     string
	InstructionFiles []string
	Checks           []Check
	// MaxRebuttals is convergence.max_rebuttals, so the model knows how
	// many more chances a disputed finding gets.
	MaxRebuttals int
	// MaxIterations caps tool-calling turns; 0 means no ceiling (§9).
	MaxIterations int
	// WrapUp is held back from ctx's deadline for a final turn that makes
	// the model submit the verdicts it has (§9). 0 disables it.
	WrapUp time.Duration
}

// VerifyResult is a verifier run's outcome. Verdicts holds the findings it
// judged, by ID; a finding without one keeps its state and is selected
// again next run. Complete is set when every check got a verdict.
type VerifyResult struct {
	Verdicts   map[string]*state.Verdict
	Usage      llms.Usage
	Reason     string
	Iterations int
	Complete   bool
}

// Unverified lists the checks that got no verdict.
func (r *VerifyResult) Unverified(checks []Check) []string {
	var out []string
	for i := range checks {
		if id := checks[i].Tracked.ID; r == nil || r.Verdicts[id] == nil {
			out = append(out, id)
		}
	}
	return out
}

const verifyTimeUp = "Time is up for this verification. Call submit_verdicts now with a verdict for every finding you " +
	"have checked. Findings you leave out are checked again on the next run. Do not investigate further."

const verifyNudge = "You ended your turn without calling submit_verdicts. Call submit_verdicts now with a verdict for " +
	"every listed finding. Do not reply in text."

// Verify runs the agentic verifier: the model checks each earlier finding
// against the code at the reviewed head, judges any replies, and ends by
// calling submit_verdicts (design §6). Like Find, it returns an error only
// for a programming or configuration fault.
func Verify(ctx context.Context, in *VerifyInput) (*VerifyResult, error) {
	if in.Client == nil || in.Workspace == nil || in.Diff == nil {
		return nil, errors.New("verify: client, workspace and diff are required")
	}
	if len(in.Checks) == 0 {
		return &VerifyResult{Verdicts: map[string]*state.Verdict{}, Complete: true}, nil
	}
	env := &toolEnv{ws: in.Workspace, diff: in.Diff}
	threads := &threadTool{checks: in.Checks}
	submit := &verdictTool{checks: in.Checks}
	st := &stage{
		client:        in.Client,
		onToolCall:    in.OnToolCall,
		beforeWrapUp:  func() { submit.partialOK = true },
		submitted:     func() bool { return submit.result != nil },
		purpose:       "buggrit.verify",
		system:        verifySystemPrompt,
		opening:       verifyOpening(in),
		nudge:         verifyNudge,
		timeUp:        verifyTimeUp,
		tools:         append(readOnlyTools(env), threads.tool()),
		terminal:      submit.tool(),
		maxIterations: in.MaxIterations,
		wrapUp:        in.WrapUp,
	}
	run, err := st.run(ctx)
	if err != nil {
		return nil, err
	}
	res := &VerifyResult{Verdicts: map[string]*state.Verdict{}, Usage: run.usage, Iterations: run.iterations, Reason: run.reason}
	if run.submitted {
		res.Verdicts = submit.result
	}
	// A submission outside the wrap-up has a verdict for every check, so
	// anything unverified comes with the run's reason for stopping.
	if n := len(res.Unverified(in.Checks)); n > 0 {
		res.Reason = fmt.Sprintf("%s; %d of %d earlier %s not verified", run.reason, n, len(in.Checks), plural(len(in.Checks), "finding"))
	} else {
		res.Complete, res.Reason = true, ""
	}
	return res, nil
}

// verifySystemPrompt is the verifier's standing instructions (design §6, §7).
const verifySystemPrompt = `You are buggrit's verifier. On earlier passes, buggrit raised findings on this pull request, each as its own review thread. The author, usually another agent such as Claude Code, then fixed them, replied, or resolved threads. For each finding listed in the opening message, decide from the code at the reviewed head commit whether the finding still holds, and judge any new reply.

How to work:
- Investigate with the tools: read_file, grep, list_dir, get_diff, and get_thread for a finding's whole thread. All tools are read-only. Read the anchored code and wherever a fix could live, such as a caller that now initializes the state.
- Batch your tool calls. The tools return almost instantly; nearly all of the time is your own thinking between calls, so whenever you know what you need next, request it all in one turn.
- An anchor can be outdated because the code moved. Use the original diff excerpt to find where that code is now.
- When you are done, call submit_verdicts exactly once, with a verdict for every listed finding. Do not answer in plain text.
- You have a time budget, stated in the opening message, and every tool result ends with the time used. Plan to submit before the wrap-up time.

Verdicts:
- fixed: the defect the finding describes is no longer present at the reviewed commit. evidence_files lists every file you relied on: the anchor file, plus wherever the fix lives. If any of them changes later, the finding is checked again.
- open: the defect is still present. reason cites the code that shows it. If a reply argues otherwise and the argument does not hold, reason answers the argument specifically.
- dismissed: only for a finding with a new reply, when the reply holds up: the behavior is deliberate and documented, the finding was mistaken, or it is out of scope by the pull request's own description. For an advisory finding (P2 or P3), any reply that gives a reason not to fix it holds up, such as "deferred to #123" or "intentional, see the ADR"; the author owns those decisions. A claim that it was fixed is not such a reason: check it, and if the code disproves it, the verdict is open.
- Judge the code, not claims about it. "Fixed in abc123" is a claim to check. A resolved thread is not evidence of a fix.
- Only judge the listed findings. Do not raise new issues here.

Writing: reason is posted as buggrit's reply in the thread, addressed to the author. One or two specific sentences, citing code where it matters.`

// verifyOpening builds the verifier's first user message.
func verifyOpening(in *VerifyInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Pull request #%d: %s\n\n", in.PR.Number, in.PR.Title)
	fmt.Fprintf(&b, "Base branch: %s. Reviewed head commit: %s.\n\n", in.PR.BaseRef, in.PR.HeadSHA)
	b.WriteString("## Description\n\n")
	if body := strings.TrimSpace(in.PR.Body); body != "" {
		b.WriteString(truncate(body, maxDescriptionBytes))
	} else {
		b.WriteString("(no description)")
	}
	b.WriteString("\n\n")
	writeInstructions(&b, in.Workspace, in.Instructions, in.InstructionFiles)

	b.WriteString("## Changed files\n\n")
	b.WriteString(fileList(in.Diff, nil, 0, maxPromptFileList))
	b.WriteString("\nCall get_diff for any file's changes.\n\n")

	// Like the finder's diff, findings are added only while the whole
	// message fits, and those that don't are named (design §5).
	fmt.Fprintf(&b, "## Findings to verify (%d)\n\n", len(in.Checks))
	var skipped []string
	for i := range in.Checks {
		var cb strings.Builder
		writeCheck(&cb, &in.Checks[i], in.MaxRebuttals)
		if b.Len()+cb.Len() > maxPromptDiffBytes-maxDeferredList {
			skipped = append(skipped, in.Checks[i].Tracked.ID)
			continue
		}
		b.WriteString(cb.String())
	}
	if len(skipped) > 0 {
		b.WriteString("Not shown, for space; read each with get_thread: ")
		for i, id := range skipped {
			if b.Len() > maxPromptDiffBytes-maxVerifyClosing {
				fmt.Fprintf(&b, "(and %d more; submit_verdicts names any you leave out)", len(skipped)-i)
				break
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(id)
		}
		b.WriteString("\n\n")
	}
	b.WriteString("Verify every finding, then call submit_verdicts once with a verdict for each.\n")
	return b.String()
}

// maxVerifyClosing is held back from the list of findings not shown, for
// the lines that close the opening message.
const maxVerifyClosing = 1 << 10

func checkIDs(checks []Check) []string {
	ids := make([]string, len(checks))
	for i := range checks {
		ids[i] = checks[i].Tracked.ID
	}
	return ids
}

// maxCheckText bounds one finding's text, and each reply, in the opening
// message, and maxPendingShown the number of replies it shows; get_thread
// has the rest.
const (
	maxCheckText    = 6 << 10
	maxPendingShown = 5
)

func writeCheck(b *strings.Builder, c *Check, maxRebuttals int) {
	t := c.Tracked
	weight := "advisory"
	if t.Blocking() {
		weight = "blocking"
	}
	fmt.Fprintf(b, "### %s (%s, %s; state %s)\n\n", t.ID, t.Severity, weight, t.State)
	fmt.Fprintf(b, "Anchor: %s. Checked because %s.", anchorText(t.Thread), c.Why)
	if t.Thread.IsResolved {
		b.WriteString(" The thread is resolved.")
	}
	b.WriteString("\n\n")
	b.WriteString(truncate(state.Strip(t.Root.Body), maxCheckText))
	b.WriteString("\n\n")
	if h := strings.TrimSpace(t.Root.DiffHunk); h != "" {
		fmt.Fprintf(b, "Diff excerpt the finding was raised on:\n\n%s\n%s\n%s\n\n", codeFenceFor(h), truncate(h, maxCheckText), codeFenceFor(h))
	}
	if len(t.Pending) > 0 {
		b.WriteString("New replies to judge:\n\n")
		for i := range t.Pending {
			if i == maxPendingShown {
				fmt.Fprintf(b, "- (%d more not shown: get_thread %s)\n", len(t.Pending)-i, t.ID)
				break
			}
			fmt.Fprintf(b, "- %s: %s\n", t.Pending[i].Author, truncate(strings.TrimSpace(t.Pending[i].Body), maxCheckText))
		}
		if t.Blocking() {
			fmt.Fprintf(b, "\nbuggrit has rebutted replies on this finding %d of %d times; after that, an open verdict sends it to a human.\n", t.Rebuttals, maxRebuttals)
		}
		b.WriteByte('\n')
	}
	if len(t.Thread.Comments) > 1+len(t.Pending) {
		fmt.Fprintf(b, "Earlier comments: get_thread %s.\n\n", t.ID)
	}
}

// anchorText describes where a thread is anchored now, and where it was.
func anchorText(th *github.Thread) string {
	switch {
	case th.FileLevel:
		return th.Path + " (the whole file)"
	case th.Line > 0 && th.Line != th.OriginalLine:
		return fmt.Sprintf("%s:%d (line %d when raised)", th.Path, th.Line, th.OriginalLine)
	case th.Line > 0:
		return fmt.Sprintf("%s:%d", th.Path, th.Line)
	}
	return fmt.Sprintf("%s, originally line %d (outdated: the code has changed since)", th.Path, th.OriginalLine)
}

// codeFenceFor is a fence that text can't close early.
func codeFenceFor(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

// threadTool is get_thread: an earlier finding's whole thread (design §5).
type threadTool struct{ checks []Check }

func (g *threadTool) tool() toolloop.Tool {
	return toolloop.Tool{Definition: llms.ToolDefinition{
		Name:        "get_thread",
		Description: "Read one of the listed findings' whole review thread: the finding, its anchor, the diff excerpt it was raised on, and every reply in order.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {"id": {"type": "string", "description": "The finding's ID, such as f-7f3a2c"}},
  "required": ["id"],
  "additionalProperties": false
}`),
	}, Execute: g.execute}
}

//nolint:gocritic // hugeParam: the signature is fixed by toolloop.Tool.Execute
func (g *threadTool) execute(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(&call, &args); err != nil {
		return toolError("%v", err)
	}
	var t *state.Tracked
	for i := range g.checks {
		if g.checks[i].Tracked.ID == strings.TrimSpace(args.ID) {
			t = g.checks[i].Tracked
		}
	}
	if t == nil {
		return toolError("no finding %q to verify; the IDs are %s", args.ID, strings.Join(checkIDs(g.checks), ", "))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s, %s, state %s. Anchor: %s.", t.ID, t.Severity, t.State, anchorText(t.Thread))
	if t.Thread.IsResolved {
		b.WriteString(" Resolved.")
	}
	b.WriteString("\n\n")
	if h := strings.TrimSpace(t.Root.DiffHunk); h != "" {
		fmt.Fprintf(&b, "Diff excerpt:\n%s\n\n", truncate(h, maxThreadComment))
	}
	// Each comment is truncated, and added only if it fits, so the result
	// stays within maxResultBytes.
	for i := range t.Thread.Comments {
		c := &t.Thread.Comments[i]
		who := c.Author
		switch {
		case i == 0:
			who += " (the finding)"
		case slices.ContainsFunc(t.Pending, func(p github.Comment) bool { return p.DatabaseID == c.DatabaseID }):
			who += " (new reply)"
		}
		entry := fmt.Sprintf("--- %s\n%s\n\n", who, truncate(strings.TrimSpace(state.Strip(c.Body)), maxThreadComment))
		if b.Len()+len(entry) > maxResultBytes {
			fmt.Fprintf(&b, "[thread truncated: %d more %s not shown]\n", len(t.Thread.Comments)-i, plural(len(t.Thread.Comments)-i, "comment"))
			break
		}
		b.WriteString(entry)
	}
	return ok(b.String())
}

// maxThreadComment bounds each comment, and the diff excerpt, in get_thread.
const maxThreadComment = 16 << 10

// maxReason bounds a verdict's reason, which is posted as a reply.
const maxReason = 4000

// verdictTool is submit_verdicts, the verifier's terminal tool. Like
// submit_findings, an invalid submission comes back with every problem
// listed (design §5).
type verdictTool struct {
	result map[string]*state.Verdict
	checks []Check
	// partialOK is set for the wrap-up turn, when a model out of time may
	// leave findings out; they're verified again next run.
	partialOK bool
}

func (s *verdictTool) tool() toolloop.Tool {
	return toolloop.Tool{Definition: llms.ToolDefinition{
		Name: "submit_verdicts",
		Description: "Submit a verdict for every listed finding, all at once, and end the verification. " +
			"If the submission is invalid you get an error listing each problem; fix them all and call submit_verdicts again.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "verdicts": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "description": "The finding's ID"},
          "state": {"type": "string", "enum": ["fixed", "open", "dismissed"]},
          "reason": {"type": "string", "description": "One or two sentences for the author, posted as the reply"},
          "evidence_files": {"type": "array", "items": {"type": "string"}, "description": "For fixed: every file the verdict relied on, including the anchor file"}
        },
        "required": ["id", "state", "reason"],
        "additionalProperties": false
      }
    }
  },
  "required": ["verdicts"],
  "additionalProperties": false
}`),
	}, Execute: s.execute}
}

type verdictArgs struct {
	ID       string   `json:"id"`
	State    string   `json:"state"`
	Reason   string   `json:"reason"`
	Evidence []string `json:"evidence_files"`
}

//nolint:gocritic // hugeParam: the signature is fixed by toolloop.Tool.Execute
func (s *verdictTool) execute(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
	var args struct {
		Verdicts []verdictArgs `json:"verdicts"`
	}
	if err := decodeArgs(&call, &args); err != nil {
		return toolError("%v. Fix the arguments and call submit_verdicts again.", err)
	}
	byID := map[string]*state.Tracked{}
	for i := range s.checks {
		byID[s.checks[i].Tracked.ID] = s.checks[i].Tracked
	}
	out := map[string]*state.Verdict{}
	var problems []string
	for i := range args.Verdicts {
		a := &args.Verdicts[i]
		id := strings.TrimSpace(a.ID)
		t := byID[id]
		switch {
		case t == nil:
			problems = append(problems, fmt.Sprintf("verdicts[%d]: %q is not a listed finding", i, a.ID))
			continue
		case out[id] != nil:
			problems = append(problems, fmt.Sprintf("verdicts[%d]: %s has more than one verdict", i, id))
			continue
		}
		v, ps := verdict(a, t)
		for _, p := range ps {
			problems = append(problems, fmt.Sprintf("verdicts[%d] (%s): %s", i, id, p))
		}
		out[id] = v
	}
	if !s.partialOK {
		var missing []string
		for i := range s.checks {
			if id := s.checks[i].Tracked.ID; out[id] == nil {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			problems = append(problems, "no verdict for "+strings.Join(missing, ", ")+"; every listed finding needs one")
		}
	}
	if len(problems) > 0 {
		return toolError("submit_verdicts was not accepted. Fix every problem below and call it again with every verdict:\n- %s",
			strings.Join(problems, "\n- "))
	}
	s.result = out
	return toolloop.ToolResult{Content: fmt.Sprintf("accepted %d verdicts", len(out)), Stop: true}, nil
}

// verdict validates one verdict against its finding.
func verdict(a *verdictArgs, t *state.Tracked) (*state.Verdict, []string) {
	var problems []string
	v := &state.Verdict{State: state.FindingState(strings.ToLower(strings.TrimSpace(a.State))), Reason: strings.TrimSpace(a.Reason)}
	switch v.State {
	case state.StateFixed:
		seen := map[string]bool{}
		for _, p := range a.Evidence {
			name, err := cleanPath(strings.TrimPrefix(strings.TrimSpace(p), "./"))
			if err != nil {
				problems = append(problems, "evidence_files: "+err.Error())
				continue
			}
			if !seen[name] {
				seen[name] = true
				v.Evidence = append(v.Evidence, name)
			}
		}
		if !seen[t.Thread.Path] {
			problems = append(problems, fmt.Sprintf("a fixed verdict needs evidence_files, including the anchor file %s", t.Thread.Path))
		}
	case state.StateDismissed:
		if len(t.Pending) == 0 {
			problems = append(problems, "dismissed is only for a finding with a new reply; this one has none, so it is fixed or open")
		}
	case state.StateOpen:
	default:
		problems = append(problems, fmt.Sprintf("state must be fixed, open or dismissed, not %q", a.State))
	}
	switch {
	case v.Reason == "":
		problems = append(problems, "reason is required")
	case len(v.Reason) > maxReason:
		problems = append(problems, fmt.Sprintf("reason must be at most %d characters", maxReason))
	}
	if v.State != state.StateFixed && len(a.Evidence) > 0 {
		problems = append(problems, "evidence_files is only for a fixed verdict")
	}
	return v, problems
}

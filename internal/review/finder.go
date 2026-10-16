package review

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/glob"
)

// FindInput is everything one finder run needs.
type FindInput struct {
	Client    llms.ChatClient
	Workspace Workspace
	Diff      *github.Diff
	// OnToolCall, if set, observes each tool call (for logs and stats).
	OnToolCall       func(toolloop.ToolCallEvent)
	PR               Context
	Instructions     string
	InstructionFiles []string
	OtherThreads     []ThreadNote
	Scope            glob.Filter
	// MaxIterations caps tool-calling turns; 0 means no ceiling (§9).
	MaxIterations int
	// WrapUp is held back from ctx's deadline for a final turn that makes
	// the model submit what it has found so far, so a pass that runs out of
	// time still publishes its work (§9). 0 disables it.
	WrapUp time.Duration
}

// FindResult is a finder run's outcome. A run that didn't finish still
// returns a result, with Complete false and Reason saying why, so the pass
// can publish "incomplete" instead of looking like "no findings" (§9). An
// incomplete run may still carry Findings: those submitted at wrap-up.
type FindResult struct {
	Usage      llms.Usage
	Summary    string
	Reason     string
	Findings   []Finding
	Iterations int
	Complete   bool
}

// timeUp is the wrap-up turn's instruction.
const timeUp = "Time is up for this review. Call submit_findings now with every finding you have confirmed so far " +
	"(an empty list if none). Do not investigate further."

// nudge is sent once if the model answers in text instead of submitting.
const nudge = "You ended your turn without calling submit_findings. Call submit_findings now with every finding " +
	"from your review (an empty list if there are none). Do not reply in text."

// Find runs the agentic finder: the model investigates with the read-only
// tools and ends by calling submit_findings (design §5). It returns an error
// only for a programming or configuration fault; model and deadline problems
// come back as an incomplete result.
func Find(ctx context.Context, in *FindInput) (*FindResult, error) {
	if in.Client == nil || in.Workspace == nil || in.Diff == nil {
		return nil, errors.New("find: client, workspace and diff are required")
	}
	env := &toolEnv{ws: in.Workspace, diff: in.Diff}
	submit := &submitTool{env: env, inScope: in.Scope.InScope}
	clock := newPassClock(ctx, time.Now(), in.WrapUp)
	tools := clock.timed(append(readOnlyTools(env), submit.tool()))

	maxIter := in.MaxIterations
	if maxIter <= 0 {
		// toolloop reads <= 0 as its small default; buggrit's 0 means none.
		// The deadline bounds the run (§9).
		maxIter = math.MaxInt
	}

	// Investigate until the deadline less the wrap-up reserve.
	investigateCtx := ctx
	if deadline, ok := ctx.Deadline(); ok && in.WrapUp > 0 {
		var cancel context.CancelFunc
		investigateCtx, cancel = context.WithDeadline(ctx, deadline.Add(-in.WrapUp))
		defer cancel()
	}
	res, out, err := investigate(investigateCtx, in, tools, submit, maxIter, clock.budget())
	if err != nil || res.Complete {
		return res, err
	}
	// The investigation ran out of time while the pass still has some: make
	// the model submit what it has. The pass stays incomplete.
	if investigateCtx != ctx && errors.Is(investigateCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		wrapUp(ctx, in, submit, res, out)
	}
	return res, nil
}

// investigate is the finder's main loop: tools until submit_findings, with
// one nudge if the model answers in text. On an incomplete run it returns
// the last outcome, whose transcript the wrap-up continues.
func investigate(ctx context.Context, in *FindInput, tools []toolloop.Tool, submit *submitTool, maxIter int, budget string) (*FindResult, *toolloop.Outcome, error) {
	res := &FindResult{}
	messages := []llms.Message{llms.UserText(openingMessage(in) + budget)}
	for attempt := 0; ; attempt++ {
		out := toolloop.Run(ctx, toolloop.Config{
			Client: in.Client,
			Request: llms.ChatRequest{
				System:   []llms.ContentPart{llms.Text(systemPrompt)},
				Messages: messages,
				Purpose:  "buggrit.find",
			},
			Tools:         tools,
			MaxIterations: maxIter - res.Iterations,
			OnToolCall:    in.OnToolCall,
		})
		addUsage(&res.Usage, &out.TotalUsage)
		res.Iterations += out.Iterations

		if out.Kind == toolloop.OutcomeTerminalTool {
			res.Findings = submit.result.Findings
			res.Summary = submit.result.Summary
			// The model's own account of coverage decides: a partial review
			// leaves the pass incomplete, so the next run covers it (§9).
			res.Complete = submit.result.Coverage == CoverageComplete
			if !res.Complete {
				res.Reason = "the model reported partial coverage"
			}
			return res, &out, nil
		}
		if out.Kind == toolloop.OutcomeFinalAnswer && attempt == 0 && res.Iterations < maxIter {
			messages = append(wellFormed(out.Messages), llms.UserText(nudge))
			continue
		}
		reason, err := incompleteReason(ctx, &out, in.MaxIterations)
		res.Reason = reason
		return res, &out, err
	}
}

// wrapUpTurns bounds the wrap-up: the forced submission plus resubmissions
// after validation errors.
const wrapUpTurns = 3

// wrapUp continues out's transcript with one instruction to submit, forcing
// submit_findings. Findings it gets are added to res, which stays
// incomplete; if it gets none, res is unchanged apart from usage.
func wrapUp(ctx context.Context, in *FindInput, submit *submitTool, res *FindResult, out *toolloop.Outcome) {
	messages := append(wellFormed(out.Messages), llms.UserText(timeUp))
	fin := toolloop.Run(ctx, toolloop.Config{
		Client: in.Client,
		Request: llms.ChatRequest{
			System:   []llms.ContentPart{llms.Text(systemPrompt)},
			Messages: messages,
			Purpose:  "buggrit.find.wrapup",
		},
		Tools:         []toolloop.Tool{submit.tool()},
		ToolChoice:    llms.ToolChoice{Type: llms.ToolChoiceTool, Name: submitFindingsDef().Name},
		MaxIterations: wrapUpTurns,
		OnToolCall:    in.OnToolCall,
	})
	addUsage(&res.Usage, &fin.TotalUsage)
	res.Iterations += fin.Iterations
	if fin.Kind != toolloop.OutcomeTerminalTool {
		return
	}
	res.Findings = submit.result.Findings
	res.Summary = submit.result.Summary
	res.Reason += fmt.Sprintf("; submitted %d %s found before it", len(res.Findings), plural(len(res.Findings), "finding"))
}

// wellFormed drops a trailing assistant turn that can't be sent back: one
// whose tool calls have no results (a run canceled mid-tool), or one with no
// content at all (a reasoning model that spent its whole output budget
// reasoning). maestro-llms rejects a transcript holding either.
func wellFormed(messages []llms.Message) []llms.Message {
	n := len(messages)
	if n == 0 || messages[n-1].Role != llms.RoleAssistant {
		return messages
	}
	if last := &messages[n-1]; hasToolCalls(last) || !hasContent(last) {
		return messages[:n-1]
	}
	return messages
}

func hasToolCalls(m *llms.Message) bool {
	for i := range m.Content {
		if m.Content[i].ToolCall != nil {
			return true
		}
	}
	return false
}

func hasContent(m *llms.Message) bool {
	for i := range m.Content {
		if m.Content[i].ToolCall != nil || strings.TrimSpace(m.Content[i].Text) != "" {
			return true
		}
	}
	return false
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// incompleteReason says why a run ended without a submission, or returns an
// error for outcomes that mean a bug rather than a model or deadline problem.
func incompleteReason(ctx context.Context, out *toolloop.Outcome, maxIterations int) (string, error) {
	deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
	switch out.Kind {
	case toolloop.OutcomeFinalAnswer:
		return "the model stopped without submitting findings", nil
	case toolloop.OutcomeMaxIterations:
		return fmt.Sprintf("tool iteration ceiling reached (%d)", maxIterations), nil
	case toolloop.OutcomeCanceled, toolloop.OutcomeLLMError:
		switch {
		case deadline:
			return "deadline reached", nil
		case out.Kind == toolloop.OutcomeCanceled:
			return "canceled", nil
		}
		return "model error: " + truncate(fmt.Sprint(out.Err), 300), nil
	case toolloop.OutcomeToolError:
		// A tool stopped by the deadline or cancellation (grep's walk checks
		// the context) is an incomplete pass, not a bug.
		switch {
		case deadline:
			return "deadline reached", nil
		case ctx.Err() != nil:
			return "canceled", nil //nolint:nilerr // cancellation is an incomplete pass, not a fault
		}
		return "", fmt.Errorf("find: %w", out.Err)
	case toolloop.OutcomeTerminalTool:
	}
	return "", fmt.Errorf("find: unexpected tool loop outcome %q", out.Kind)
}

func addUsage(total, u *llms.Usage) {
	total.InputTokens += u.InputTokens
	total.OutputTokens += u.OutputTokens
	total.TotalTokens += u.TotalTokens
	total.ReasoningTokens += u.ReasoningTokens
	total.BillableOutputTokens += u.BillableOutputTokens
	total.CacheReadTokens += u.CacheReadTokens
	total.CacheWriteTokens += u.CacheWriteTokens
}

// truncate shortens s to at most n bytes without splitting a UTF-8 sequence,
// marking the cut with "…".
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

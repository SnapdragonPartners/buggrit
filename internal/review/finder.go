package review

import (
	"context"
	"errors"
	"fmt"
	"math"
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
}

// FindResult is a finder run's outcome. A run that didn't finish still
// returns a result, with Complete false and Reason saying why, so the pass
// can publish "incomplete" instead of looking like "no findings" (§9).
type FindResult struct {
	Usage      llms.Usage
	Summary    string
	Reason     string
	Findings   []Finding
	Iterations int
	Complete   bool
}

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
	tools := append(readOnlyTools(env), submit.tool())

	maxIter := in.MaxIterations
	if maxIter <= 0 {
		// toolloop reads <= 0 as its small default; buggrit's 0 means none.
		// The deadline bounds the run (§9).
		maxIter = math.MaxInt
	}

	res := &FindResult{}
	messages := []llms.Message{llms.UserText(openingMessage(in))}
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
			res.Complete = true
			res.Findings = submit.result.Findings
			res.Summary = submit.result.Summary
			return res, nil
		}
		if out.Kind == toolloop.OutcomeFinalAnswer && attempt == 0 && res.Iterations < maxIter {
			messages = out.Messages
			messages = append(messages, llms.UserText(nudge))
			continue
		}
		reason, err := incompleteReason(ctx, &out, in.MaxIterations)
		res.Reason = reason
		return res, err
	}
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

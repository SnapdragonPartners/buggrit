package review

import (
	"context"
	"errors"
	"fmt"
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
	// Incremental, if set, limits where new findings may be anchored to the
	// lines changed since the last completed pass (§7 rule 2).
	Incremental *Incremental
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
	submit := &submitTool{env: env, inScope: in.Scope.InScope, incremental: in.Incremental}
	st := &stage{
		client:        in.Client,
		onToolCall:    in.OnToolCall,
		submitted:     func() bool { return submit.result != nil },
		purpose:       "buggrit.find",
		system:        systemPrompt,
		opening:       openingMessage(in),
		nudge:         nudge,
		timeUp:        timeUp,
		tools:         readOnlyTools(env),
		terminal:      submit.tool(),
		maxIterations: in.MaxIterations,
		wrapUp:        in.WrapUp,
	}
	run, err := st.run(ctx)
	if err != nil {
		return nil, err
	}
	res := &FindResult{Usage: run.usage, Iterations: run.iterations, Reason: run.reason}
	if !run.submitted {
		return res, nil
	}
	res.Findings, res.Summary = submit.result.Findings, submit.result.Summary
	switch {
	case run.wrappedUp:
		// Submitted only when told time was up: the pass stays incomplete.
		res.Reason += fmt.Sprintf("; submitted %d %s found before it", len(res.Findings), plural(len(res.Findings), "finding"))
	case submit.result.Coverage == CoverageComplete:
		res.Complete = true
	default:
		// The model's own account of coverage decides: a partial review
		// leaves the pass incomplete, so the next run covers it (§9).
		res.Reason = "the model reported partial coverage"
	}
	return res, nil
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// reasonDeadline is the incomplete reason for a run the deadline stopped.
const reasonDeadline = "deadline reached"

// incompleteReason says why a run ended without a submission, or returns an
// error for outcomes that mean a bug rather than a model or deadline problem.
func incompleteReason(ctx context.Context, out *toolloop.Outcome, maxIterations int, terminal string) (string, error) {
	deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
	switch out.Kind {
	case toolloop.OutcomeFinalAnswer:
		return "the model stopped without calling " + terminal, nil
	case toolloop.OutcomeEmptyResponse:
		return fmt.Sprintf("the model returned an empty response (stop reason %q)", out.Response.StopReason), nil
	case toolloop.OutcomeMaxIterations:
		return fmt.Sprintf("tool iteration ceiling reached (%d)", maxIterations), nil
	case toolloop.OutcomeDeadline:
		return reasonDeadline, nil
	case toolloop.OutcomeCanceled, toolloop.OutcomeLLMError:
		switch {
		case deadline:
			return reasonDeadline, nil
		case out.Kind == toolloop.OutcomeCanceled:
			return "canceled", nil
		}
		return "model error: " + truncate(fmt.Sprint(out.Err), 300), nil
	case toolloop.OutcomeContextExceeded:
		// toolloop already lets a done context win over the overflow.
		return "context window exceeded: " + truncate(fmt.Sprint(out.Err), 300), nil
	case toolloop.OutcomeToolError:
		// A tool stopped by the deadline or cancellation (grep's walk checks
		// the context) is an incomplete pass, not a bug.
		switch {
		case deadline:
			return reasonDeadline, nil
		case ctx.Err() != nil:
			return "canceled", nil //nolint:nilerr // cancellation is an incomplete pass, not a fault
		}
		return "", fmt.Errorf("%s: %w", terminal, out.Err)
	case toolloop.OutcomeTerminalTool, toolloop.OutcomeCompactionError:
		// buggrit sets no Compact hook, so compaction can't fail.
	}
	return "", fmt.Errorf("%s: unexpected tool loop outcome %q", terminal, out.Kind)
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

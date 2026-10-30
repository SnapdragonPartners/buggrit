package review

import (
	"context"
	"errors"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/telemetry"
)

// stage is one agentic stage, the finder or the verifier: a toolloop run
// over the read-only tools that ends when the model calls the stage's
// terminal tool (design §5). The tool loop lives here, in one function.
type stage struct {
	client llms.ChatClient
	// onToolCall, if set, observes each tool call.
	onToolCall func(toolloop.ToolCallEvent)
	// session records the run's telemetry; nil records nothing.
	session *telemetry.Session
	// packing is what the opening message carried, for telemetry.
	packing *packing
	// beforeWrapUp, if set, runs before the wrap-up turn, so the terminal
	// tool can relax what it accepts from a model that's out of time.
	beforeWrapUp func()
	// submitted reports whether the terminal tool accepted a submission.
	submitted func() bool
	purpose   llms.Purpose
	// prompts are the standing instructions: prompts.Nudge is sent once if
	// the model answers in text, and prompts.TimeUp is the wrap-up turn's
	// instruction.
	prompts stagePrompts
	// opening is the first user message, without the time budget.
	opening string
	// tools are the read-only tools; terminal is the stage's terminal tool.
	tools    []toolloop.Tool
	terminal toolloop.Tool
	// maxIterations caps tool-calling turns; 0 means no ceiling (§9).
	maxIterations int
	// wrapUp is held back from ctx's deadline for a final turn that makes the
	// model submit what it has (§9). 0 disables it.
	wrapUp time.Duration
}

// stageRun is how a stage's run went. A run that ended without a
// submission says why in reason; one that submitted only at wrap-up has
// both submitted and wrappedUp set, and reason says why it needed one.
type stageRun struct {
	usage      llms.Usage
	reason     string
	iterations int
	submitted  bool
	wrappedUp  bool
}

// wrapUpTurns bounds the wrap-up: the forced submission plus resubmissions
// after validation errors.
const wrapUpTurns = 3

// run runs the stage. It returns an error only for a programming or
// configuration fault; model and deadline problems come back in the run.
func (s *stage) run(ctx context.Context) (*stageRun, error) {
	s.client = s.session.Chat(s.client)
	if s.session != nil {
		s.session.Emit(telemetry.KindOpening, s.packing.record())
		observe := s.onToolCall
		s.onToolCall = func(e toolloop.ToolCallEvent) { //nolint:gocritic // hugeParam: fixed by toolloop
			s.session.Tool(e)
			if observe != nil {
				observe(e)
			}
		}
	}
	res, err := s.runStage(ctx)
	if res != nil {
		s.session.Emit(telemetry.KindStage, stageRecord{
			Reason: res.reason, Iterations: res.iterations, Submitted: res.submitted, WrappedUp: res.wrappedUp, Usage: res.usage,
		})
	}
	return res, err
}

// stageRecord is a stage run's telemetry.
type stageRecord struct {
	Reason     string     `json:"reason,omitempty"`
	Usage      llms.Usage `json:"usage"`
	Iterations int        `json:"iterations"`
	Submitted  bool       `json:"submitted"`
	WrappedUp  bool       `json:"wrapped_up"`
}

func (s *stage) runStage(ctx context.Context) (*stageRun, error) {
	clock := newPassClock(ctx, time.Now(), s.wrapUp)
	tools := clock.timed(append(append([]toolloop.Tool(nil), s.tools...), s.terminal))
	maxIter := s.maxIterations
	if maxIter <= 0 {
		// toolloop reads 0 as its small default; buggrit's 0 means none.
		// The deadline bounds the run (§9).
		maxIter = toolloop.NoIterationLimit
	}

	// Investigate until the deadline less the wrap-up reserve.
	investigateCtx := ctx
	if deadline, ok := ctx.Deadline(); ok && s.wrapUp > 0 {
		var cancel context.CancelFunc
		investigateCtx, cancel = context.WithDeadline(ctx, deadline.Add(-s.wrapUp))
		defer cancel()
	}
	res, out, err := s.investigate(investigateCtx, tools, maxIter, clock.budget())
	if err != nil || res.submitted {
		return res, err
	}
	// The investigation ran out of time while the pass still has some: make
	// the model submit what it has.
	if investigateCtx != ctx && errors.Is(investigateCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		s.wrapUpTurn(ctx, res, out)
	}
	return res, nil
}

// investigate is the main loop: tools until the terminal tool, with one
// nudge if the model answers in text or comes back empty. On a run without a submission it
// returns the last outcome, whose transcript the wrap-up continues.
func (s *stage) investigate(ctx context.Context, tools []toolloop.Tool, maxIter int, budget string) (*stageRun, *toolloop.Outcome, error) {
	res := &stageRun{}
	messages := []llms.Message{llms.UserText(s.opening + budget)}
	for attempt := 0; ; attempt++ {
		phase := "investigate"
		if attempt > 0 {
			phase = "nudge"
		}
		s.session.SetPhase(phase)
		out := toolloop.Run(ctx, toolloop.Config{
			Client: s.client,
			Request: llms.ChatRequest{
				System:   []llms.ContentPart{llms.Text(s.prompts.System)},
				Messages: messages,
				Purpose:  s.purpose,
			},
			Tools:         tools,
			MaxIterations: remaining(maxIter, res.iterations),
			OnToolCall:    s.onToolCall,
		})
		addUsage(&res.usage, &out.TotalUsage)
		res.iterations += out.Iterations

		if out.Kind == toolloop.OutcomeTerminalTool {
			res.submitted = s.submitted()
			return res, &out, nil
		}
		stopped := out.Kind == toolloop.OutcomeFinalAnswer || out.Kind == toolloop.OutcomeEmptyResponse
		if stopped && attempt == 0 && remaining(maxIter, res.iterations) != 0 {
			messages = append(wellFormed(out.Messages), llms.UserText(s.prompts.Nudge))
			continue
		}
		reason, err := incompleteReason(ctx, &out, s.maxIterations, s.terminal.Definition.Name)
		res.reason = reason
		return res, &out, err
	}
}

// wrapUpTurn continues out's transcript with one instruction to submit,
// forcing the terminal tool.
func (s *stage) wrapUpTurn(ctx context.Context, res *stageRun, out *toolloop.Outcome) {
	if s.beforeWrapUp != nil {
		s.beforeWrapUp()
	}
	messages := append(wellFormed(out.Messages), llms.UserText(s.prompts.TimeUp))
	s.session.SetPhase("wrapup")
	fin := toolloop.Run(ctx, toolloop.Config{
		Client: s.client,
		Request: llms.ChatRequest{
			System:   []llms.ContentPart{llms.Text(s.prompts.System)},
			Messages: messages,
			Purpose:  s.purpose + ".wrapup",
		},
		Tools:         []toolloop.Tool{s.terminal},
		ToolChoice:    llms.ToolChoice{Type: llms.ToolChoiceTool, Name: s.terminal.Definition.Name},
		MaxIterations: wrapUpTurns,
		OnToolCall:    s.onToolCall,
	})
	addUsage(&res.usage, &fin.TotalUsage)
	res.iterations += fin.Iterations
	if fin.Kind == toolloop.OutcomeTerminalTool && s.submitted() {
		res.submitted, res.wrappedUp = true, true
	}
}

// remaining is the iteration limit left for the next toolloop run: 0 when
// the ceiling is spent, NoIterationLimit when there is none.
func remaining(maxIter, used int) int {
	if maxIter < 0 {
		return toolloop.NoIterationLimit
	}
	return maxIter - used
}

// wellFormed drops a trailing assistant turn whose tool calls have no
// results, which a run canceled mid-tool leaves behind and maestro-llms
// rejects. Dropping the last turn leaves the reasoning in earlier turns
// valid: only an edit before a reasoning part invalidates it.
func wellFormed(messages []llms.Message) []llms.Message {
	n := len(messages)
	if n == 0 || messages[n-1].Role != llms.RoleAssistant {
		return messages
	}
	if hasToolCalls(&messages[n-1]) {
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

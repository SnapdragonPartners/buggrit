package review

import (
	"context"
	"fmt"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// passClock tells the model how much of the pass's time it has used, so it
// can plan to submit before the wrap-up (design §9). A pass without a
// deadline has no clock; its methods are then no-ops.
type passClock struct {
	start, deadline, wrapUpAt time.Time
	now                       func() time.Time
}

// newPassClock returns the clock for a pass that started at start, or nil
// if ctx has no deadline.
func newPassClock(ctx context.Context, start time.Time, wrapUp time.Duration) *passClock {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	return &passClock{start: start, deadline: deadline, wrapUpAt: deadline.Add(-wrapUp), now: time.Now}
}

// budget is the opening message's statement of the time available.
func (c *passClock) budget() string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("\n## Time\n\nYou have about %s for this review. Submit by %s; after that you will be asked to submit what you have. "+
		"Each tool result ends with the time used.\n", minutes(c.deadline.Sub(c.start)), minutes(c.wrapUpAt.Sub(c.start)))
}

// footer is appended to each tool result.
func (c *passClock) footer() string {
	return fmt.Sprintf("[time: %s of %s used; submit by %s]",
		minutes(c.now().Sub(c.start)), minutes(c.deadline.Sub(c.start)), minutes(c.wrapUpAt.Sub(c.start)))
}

// timed wraps each tool so its results end with the footer. A terminal
// result isn't changed; the loop ends with it.
func (c *passClock) timed(tools []toolloop.Tool) []toolloop.Tool {
	if c == nil {
		return tools
	}
	out := make([]toolloop.Tool, len(tools))
	for i := range tools {
		t := tools[i]
		exec := t.Execute
		t.Execute = func(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) { //nolint:gocritic // hugeParam: fixed by toolloop.Tool.Execute
			r, err := exec(ctx, call)
			if err == nil && !r.Stop {
				r.Content += "\n\n" + c.footer()
			}
			return r, err
		}
		out[i] = t
	}
	return out
}

// minutes renders a duration for the model: whole minutes, or seconds when
// short.
func minutes(d time.Duration) string {
	if d < 2*time.Minute {
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	}
	return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
}

package telemetry

import (
	"encoding/json"
	"fmt"
)

// Rebuild returns the messages of a session's call n, each as its message
// event recorded it, from a run's events in any order. It is the reference
// for docs/telemetry.md's query: for each position below the call's
// message count, the message written there by the latest call up to n.
func Rebuild(events []*Event, session string, n int) ([]json.RawMessage, error) {
	count, err := messageCount(events, session, n)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, count)
	from := make([]int, count)
	for _, e := range events {
		if e.Kind != KindMessage || e.Session != session || e.Call > n {
			continue
		}
		var m messageEvent
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			return nil, fmt.Errorf("message event %d: %w", e.Seq, err)
		}
		if m.Pos < count && e.Call >= from[m.Pos] {
			out[m.Pos], from[m.Pos] = m.Message, e.Call
		}
	}
	for i := range out {
		if out[i] == nil {
			return nil, fmt.Errorf("call %d: no message at position %d", n, i)
		}
	}
	return out, nil
}

// messageCount is how many messages a session's call n sent.
func messageCount(events []*Event, session string, n int) (int, error) {
	for _, e := range events {
		if e.Kind == KindCall && e.Session == session && e.Call == n {
			var c struct {
				Messages int `json:"messages"`
			}
			if err := json.Unmarshal(e.Payload, &c); err != nil {
				return 0, fmt.Errorf("call %d: %w", n, err)
			}
			return c.Messages, nil
		}
	}
	return 0, fmt.Errorf("no call %d in session %q", n, session)
}

// messageEvent is a message event's payload as Rebuild reads it.
type messageEvent struct {
	Message json.RawMessage `json:"message"`
	Pos     int             `json:"pos"`
}

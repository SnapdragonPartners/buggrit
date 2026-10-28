// Package telemetry records what a review run did, for comparing models and
// tuning what the opening request carries (docs/design.md §6, "Telemetry";
// docs/telemetry.md). It is raw data for optimization, not a forensic
// record: a run that couldn't write every event says so, and goes on.
//
// The engine records through a *Recorder, which knows nothing of where
// events go. A nil *Recorder, or a nil *Session, records nothing, so the
// engine never checks whether telemetry is on. The front end gives the
// Recorder a Writer, which batches events to a Sink off the callback path.
//
// A stage run is one session. Each model call in it records the messages it
// adds to the session's transcript, not the whole request: buggrit never
// compacts, so a request is the previous one's first Keep messages plus
// those it adds, and any request can be rebuilt in order (docs/telemetry.md).
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// Event kinds. docs/telemetry.md describes each payload.
const (
	KindPass    = "pass"    // the pass's inputs
	KindOpening = "opening" // what a stage's opening message carried
	KindMessage = "message" // a message a call added to its session's transcript
	KindCall    = "call"    // one logical model call and its response
	KindAttempt = "attempt" // one provider attempt of a call
	KindTool    = "tool"    // one tool call
	KindStage   = "stage"   // how a stage run ended
	KindResult  = "result"  // what the pass decided
	KindPublish = "publish" // what publishing did
)

// Event is one recorded observation. Session and Call are empty for events
// outside a session or a call. Seq is set by the Writer.
type Event struct {
	At      time.Time
	Kind    string
	Session string
	Payload json.RawMessage
	Call    int
	Seq     int
}

// Recorder takes one run's events.
type Recorder struct {
	emit func(*Event)
	now  func() time.Time
}

// NewRecorder returns a Recorder that hands each event to emit, which must
// not block.
func NewRecorder(emit func(*Event)) *Recorder {
	return &Recorder{emit: emit, now: time.Now}
}

// Emit records an event outside any session.
func (r *Recorder) Emit(kind string, payload any) { r.record(kind, "", 0, payload) }

func (r *Recorder) record(kind, session string, call int, payload any) {
	if r == nil {
		return
	}
	r.emit(&Event{At: r.now(), Kind: kind, Session: session, Call: call, Payload: marshal(payload)})
}

// marshal encodes a payload as JSON that Postgres' jsonb accepts. A payload
// that can't be encoded is recorded as the error, rather than lost.
func marshal(payload any) json.RawMessage {
	data, err := json.Marshal(payload)
	if err != nil {
		data, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	return jsonbSafe(data)
}

// jsonbSafe replaces each \u0000 escape in encoded JSON with �: jsonb
// rejects NUL, and a tool result read from a binary file can hold one.
// Walking escapes from the start keeps an escaped backslash followed by the
// text "u0000" intact.
func jsonbSafe(data []byte) []byte {
	var out []byte
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' || i+1 >= len(data) {
			continue
		}
		if data[i+1] == 'u' && i+6 <= len(data) && string(data[i+2:i+6]) == "0000" {
			if out == nil {
				out = slices.Clone(data)
			}
			copy(out[i+2:i+6], "fffd")
		}
		i++ // skip the escaped character
	}
	if out == nil {
		return data
	}
	return out
}

// Attempt records one provider attempt of a model call, from maestro-llms'
// metrics middleware, with the call the attempt's context names. Its
// error's text is kept: the database is the org's own, and the text is
// what explains a failure.
//
//nolint:gocritic // hugeParam: e comes from middleware.ContextObserver
func (r *Recorder) Attempt(ctx context.Context, e middleware.Event) {
	if r == nil {
		return
	}
	ref, _ := CallFrom(ctx)
	n, _ := middleware.AttemptFromContext(ctx)
	r.record(KindAttempt, ref.Session, ref.Call, attemptRecord{
		Attempt: n, Provider: string(e.Provider), Model: e.Model, Purpose: string(e.Purpose),
		LatencyMS: e.Latency.Milliseconds(), Usage: e.Usage, Error: errText(e.Err), ErrorKind: errKind(e.Err),
	})
}

type attemptRecord struct {
	Provider  string     `json:"provider"`
	Model     string     `json:"model"`
	Purpose   string     `json:"purpose"`
	Error     string     `json:"error,omitempty"`
	ErrorKind string     `json:"error_kind,omitempty"`
	Usage     llms.Usage `json:"usage"`
	Attempt   int        `json:"attempt"`
	LatencyMS int64      `json:"latency_ms"`
}

// CallRef names one model call: its session and its number there.
type CallRef struct {
	Session string
	Call    int
}

type callKey struct{}

// CallFrom returns the model call a context belongs to, which a Session's
// chat client puts there.
func CallFrom(ctx context.Context) (CallRef, bool) {
	ref, ok := ctx.Value(callKey{}).(CallRef)
	return ref, ok
}

// Session records one stage run.
type Session struct {
	rec  *Recorder
	defs map[string]llms.ToolDefinition
	name string
	// phase is what the stage is doing: investigate, nudge or wrapup.
	phase string
	// sent is the last request's messages, for the next call's delta.
	sent  []llms.Message
	calls int
	mu    sync.Mutex
}

// Session starts recording a stage run named name.
func (r *Recorder) Session(name string) *Session {
	if r == nil {
		return nil
	}
	return &Session{rec: r, name: name, defs: map[string]llms.ToolDefinition{}}
}

// SetPhase labels the calls that follow.
func (s *Session) SetPhase(phase string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.phase = phase
	s.mu.Unlock()
}

// Emit records an event in the session, with the call it follows.
func (s *Session) Emit(kind string, payload any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	call := s.calls
	s.mu.Unlock()
	s.rec.record(kind, s.name, call, payload)
}

// Tool records a tool call. It belongs to the call whose response asked
// for it: a session's tools run after that response and before the next
// call.
//
//nolint:gocritic // hugeParam: e comes from toolloop's OnToolCall
func (s *Session) Tool(e toolloop.ToolCallEvent) {
	s.Emit(KindTool, toolRecord{
		Iteration: e.Iteration, ID: e.Call.ID, Name: e.Call.Name, Parameters: e.Call.Parameters,
		Result: e.Result.Content, IsError: e.Result.IsError, Stop: e.Result.Stop,
		Error: errText(e.Err), LatencyMS: e.Latency.Milliseconds(),
	})
}

type toolRecord struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Result     string          `json:"result"`
	Error      string          `json:"error,omitempty"`
	Parameters json.RawMessage `json:"parameters"`
	Iteration  int             `json:"iteration"`
	LatencyMS  int64           `json:"latency_ms"`
	IsError    bool            `json:"is_error,omitempty"`
	Stop       bool            `json:"stop,omitempty"`
}

// Chat wraps a stage's client so each call records its delta, its request
// settings and its response. With a nil Session it returns c.
func (s *Session) Chat(c llms.ChatClient) llms.ChatClient {
	if s == nil {
		return c
	}
	return &chat{ChatClient: c, s: s}
}

type chat struct {
	llms.ChatClient
	s *Session
}

// Complete records the call around the wrapped client. The signature is
// fixed by llms.ChatClient.
//
//nolint:gocritic // hugeParam: req is passed by value to satisfy the interface
func (c *chat) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	s := c.s
	s.mu.Lock()
	s.calls++
	n, phase := s.calls, s.phase
	keep := commonPrefix(s.sent, req.Messages)
	// A copy: a caller that cuts its transcript and appends reuses the
	// backing array, which would rewrite what was sent.
	s.sent = slices.Clone(req.Messages)
	defs := s.newDefs(req.Tools)
	s.mu.Unlock()

	for i, m := range req.Messages[keep:] {
		s.rec.record(KindMessage, s.name, n, messageRecord{Pos: keep + i, Message: m})
	}
	start := s.rec.now()
	resp, err := c.ChatClient.Complete(context.WithValue(ctx, callKey{}, CallRef{Session: s.name, Call: n}), req)
	rec := callRecord{
		Phase: phase, Purpose: string(req.Purpose), Keep: keep, Messages: len(req.Messages),
		Tools: toolNames(req.Tools), ToolDefs: defs, ToolChoice: req.ToolChoice,
		MaxTokens: req.MaxTokens, Reasoning: req.Reasoning,
		LatencyMS: s.rec.now().Sub(start).Milliseconds(), Error: errText(err), ErrorKind: errKind(err),
	}
	if len(req.System) > 0 && keep == 0 {
		rec.System = req.System
	}
	if err == nil {
		rec.Response = &responseRecord{
			Message: resp.Message, StopReason: string(resp.StopReason),
			Usage: resp.Usage, AppliedReasoning: resp.AppliedReasoning,
		}
	}
	s.rec.record(KindCall, s.name, n, rec)
	return resp, err //nolint:wrapcheck // a transparent decorator
}

// newDefs returns the tool definitions this session hasn't recorded yet, or
// that changed, and records them. Callers hold s.mu.
func (s *Session) newDefs(tools []llms.ToolDefinition) []llms.ToolDefinition {
	out := make([]llms.ToolDefinition, 0, len(tools))
	for i := range tools {
		d := &tools[i]
		if old, ok := s.defs[d.Name]; ok && reflect.DeepEqual(old, *d) {
			continue
		}
		s.defs[d.Name] = *d
		out = append(out, *d)
	}
	return out
}

// commonPrefix is how many leading messages two transcripts share.
// Messages a caller kept share their content slices, so the comparison is
// cheap.
func commonPrefix(a, b []llms.Message) int {
	n := 0
	for n < len(a) && n < len(b) && reflect.DeepEqual(a[n], b[n]) {
		n++
	}
	return n
}

func toolNames(tools []llms.ToolDefinition) []string {
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].Name
	}
	return names
}

type messageRecord struct {
	Message llms.Message `json:"message"`
	Pos     int          `json:"pos"`
}

// callRecord is one logical call. Keep is how many of the previous call's
// messages this request kept; the messages it added are its message events.
// System is set on a session's first call, when nothing is kept. ToolDefs
// are the definitions first offered, or changed, in this call; Tools names
// every tool offered.
type callRecord struct {
	Reasoning  *llms.ReasoningConfig `json:"reasoning,omitempty"`
	Response   *responseRecord       `json:"response,omitempty"`
	ToolChoice llms.ToolChoice       `json:"tool_choice"`
	Phase      string                `json:"phase"`
	Purpose    string                `json:"purpose"`
	Error      string                `json:"error,omitempty"`
	ErrorKind  string                `json:"error_kind,omitempty"`
	System     []llms.ContentPart    `json:"system,omitempty"`
	Tools      []string              `json:"tools"`
	ToolDefs   []llms.ToolDefinition `json:"tool_defs,omitempty"`
	Keep       int                   `json:"keep"`
	Messages   int                   `json:"messages"`
	MaxTokens  int                   `json:"max_tokens,omitempty"`
	LatencyMS  int64                 `json:"latency_ms"`
}

// responseRecord is a response without its provider-specific Raw data,
// which is outside maestro-llms' stability contract and may not encode.
type responseRecord struct {
	AppliedReasoning *llms.ReasoningConfig `json:"applied_reasoning,omitempty"`
	Usage            llms.Usage            `json:"usage"`
	StopReason       string                `json:"stop_reason"`
	Message          llms.Message          `json:"message"`
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// errKind is a provider error's kind, "other" for any other error, and ""
// for none.
func errKind(err error) string {
	var pe *llms.ProviderError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &pe):
		return string(pe.Kind)
	}
	return "other"
}

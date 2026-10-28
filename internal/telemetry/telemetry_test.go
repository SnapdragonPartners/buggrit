package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

// collector keeps every event, numbered in the order it got them.
type collector struct {
	events []*Event
	mu     sync.Mutex
}

func (c *collector) emit(e *Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.Seq = len(c.events) + 1
	c.events = append(c.events, e)
}

func (c *collector) all() []*Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*Event(nil), c.events...)
}

func mustRebuild(t *testing.T, events []*Event, session string, n int) []json.RawMessage {
	t.Helper()
	msgs, err := Rebuild(events, session, n)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// messagesJSON encodes each message the way a message event does.
func messagesJSON(t *testing.T, msgs []llms.Message) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(msgs))
	for i := range msgs {
		out[i] = marshal(msgs[i])
	}
	return out
}

func sameMessages(t *testing.T, got, want []json.RawMessage) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("rebuilt %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != string(want[i]) {
			t.Errorf("message %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

func callRecords(t *testing.T, events []*Event, session string) []callRecord {
	t.Helper()
	var out []callRecord
	for _, e := range events {
		if e.Kind == KindCall && e.Session == session {
			var c callRecord
			if err := json.Unmarshal(e.Payload, &c); err != nil {
				t.Fatal(err)
			}
			out = append(out, c)
		}
	}
	return out
}

// A session records each call as the messages it keeps and adds, and every
// request rebuilds exactly: appends, and a transcript cut back and appended
// in its own backing array, as a nudge after a dangling tool call does.
func TestSessionRecordsDeltas(t *testing.T) {
	c := &collector{}
	s := NewRecorder(c.emit).Session("find")
	fake := &testllm.FakeChatClient{Text: "ok"}
	client := s.Chat(fake)

	u0 := llms.UserText("review this")
	a1 := llms.Message{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "t1", Name: "read_file", Parameters: json.RawMessage(`{"path": "a.go"}`)}}}}
	r1 := llms.Message{Role: llms.RoleTool, Content: []llms.ContentPart{{Type: llms.ContentToolResult, ToolResult: &llms.ToolResult{ToolCallID: "t1", Content: "package a\x00"}}}}
	a2 := llms.Message{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "t2", Name: "grep"}}}}
	nudge := llms.UserText("call submit_findings")

	backing := make([]llms.Message, 0, 8)
	steps := []func() []llms.Message{
		func() []llms.Message { return append(backing, u0) },
		func() []llms.Message { return append(backing, u0, a1, r1) },
		func() []llms.Message { return append(backing, u0, a1, r1, a2) },
		func() []llms.Message { return append(backing[:3], nudge) }, // overwrites a2 in place
	}
	tools := []llms.ToolDefinition{{Name: "read_file", Description: "read"}, {Name: "grep", Description: "search"}}
	for i, step := range steps {
		req := llms.ChatRequest{System: []llms.ContentPart{llms.Text("system")}, Messages: step(), Tools: tools, Purpose: "buggrit.find"}
		if i == 3 {
			s.SetPhase("nudge")
			req.Tools = tools[:1]
		}
		if _, err := client.Complete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}

	events := c.all()
	calls := callRecords(t, events, "find")
	if len(calls) != 4 {
		t.Fatalf("recorded %d calls, want 4", len(calls))
	}
	for i, want := range []int{0, 1, 3, 3} {
		if calls[i].Keep != want {
			t.Errorf("call %d keeps %d messages, want %d", i+1, calls[i].Keep, want)
		}
	}
	if len(calls[0].System) == 0 || len(calls[1].System) != 0 {
		t.Errorf("the system prompt should be recorded on the first call only")
	}
	if len(calls[0].ToolDefs) != 2 || len(calls[1].ToolDefs) != 0 || len(calls[3].Tools) != 1 {
		t.Errorf("tool definitions should be recorded once, and every call lists its tools: %+v", calls)
	}
	if calls[3].Phase != "nudge" || calls[0].Response == nil || calls[0].Response.Message.Content[0].Text != "ok" {
		t.Errorf("call records: %+v", calls)
	}
	sent := fake.Calls()
	for n := 1; n <= 4; n++ {
		sameMessages(t, mustRebuild(t, events, "find", n), messagesJSON(t, sent[n-1].Messages))
	}
}

// The context a call passes down names the call, so the attempts maestro-
// llms' middleware observes, retries included, are tied to it.
func TestAttemptsNameTheirCall(t *testing.T) {
	c := &collector{}
	rec := NewRecorder(c.emit)
	tries := 0
	fake := &testllm.FakeChatClient{Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
		if ref, ok := CallFrom(ctx); !ok || ref != (CallRef{Session: "verify", Call: 1}) {
			t.Errorf("the provider's context names %+v, %v", ref, ok)
		}
		tries++
		if tries == 1 {
			return llms.ChatResponse{}, &llms.ProviderError{Kind: llms.ErrorKindUnavailable, Message: "busy"}
		}
		return testllm.TextResponse("ok"), nil
	}}
	chain := middleware.RecommendedChat(fake, middleware.RecommendedConfig{
		Observer: attemptObserver{rec},
		Retry:    middleware.RetryConfig{MaxAttempts: 3, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	client := rec.Session("verify").Chat(chain)
	if _, err := client.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}}); err != nil {
		t.Fatal(err)
	}
	var attempts []attemptRecord
	for _, e := range c.all() {
		if e.Kind != KindAttempt {
			continue
		}
		if e.Session != "verify" || e.Call != 1 {
			t.Errorf("attempt event in %q call %d", e.Session, e.Call)
		}
		var a attemptRecord
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, a)
	}
	if len(attempts) != 2 || attempts[0].Attempt != 1 || attempts[0].ErrorKind != "unavailable" || attempts[1].Attempt != 2 || attempts[1].Error != "" {
		t.Errorf("attempts = %+v", attempts)
	}
}

type attemptObserver struct{ rec *Recorder }

func (o attemptObserver) ObserveContext(ctx context.Context, e middleware.Event) { //nolint:gocritic // fixed by the interface
	o.rec.Attempt(ctx, e)
}
func (attemptObserver) Observe(middleware.Event) {} //nolint:gocritic // fixed by the interface

// Concurrent sessions, as the finder and the verifier are, keep their own
// call numbers and transcripts.
func TestConcurrentSessionsDontCollide(t *testing.T) {
	c := &collector{}
	rec := NewRecorder(c.emit)
	fakes := map[string]*testllm.FakeChatClient{"find": {Text: "f"}, "verify": {Text: "v"}}
	var wg sync.WaitGroup
	for name, fake := range fakes {
		client := rec.Session(name).Chat(fake)
		wg.Go(func() {
			var msgs []llms.Message
			for i := range 20 {
				msgs = append(msgs, llms.UserText(fmt.Sprintf("%s %d", name, i)))
				if _, err := client.Complete(context.Background(), llms.ChatRequest{Messages: msgs}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	events := c.all()
	for name, fake := range fakes {
		if n := len(callRecords(t, events, name)); n != 20 {
			t.Errorf("%s: %d calls, want 20", name, n)
		}
		for n, req := range fake.Calls() {
			sameMessages(t, mustRebuild(t, events, name, n+1), messagesJSON(t, req.Messages))
		}
	}
}

// A nil Recorder or Session records nothing and passes calls through.
func TestNilRecorderIsANoOp(t *testing.T) {
	var rec *Recorder
	rec.Emit(KindPass, nil)
	rec.Attempt(context.Background(), middleware.Event{})
	s := rec.Session("find")
	s.SetPhase("x")
	s.Emit(KindStage, nil)
	fake := &testllm.FakeChatClient{Text: "ok"}
	if got := s.Chat(fake); got != llms.ChatClient(fake) {
		t.Errorf("a nil session should return the client itself")
	}
}

func TestJSONBSafe(t *testing.T) {
	for in, want := range map[string]string{
		`"a\u0000b"`:      `"a\ufffdb"`,
		`"a\\u0000b"`:     `"a\\u0000b"`, // an escaped backslash, then text
		`"a\\\u0000b"`:    `"a\\\ufffdb"`,
		`"\u00001\u0000"`: `"\ufffd1\ufffd"`,
		`"plain \u0001"`:  `"plain \u0001"`,
		`{"k":"\u0000"}`:  `{"k":"\ufffd"}`,
		`"trailing \\"`:   `"trailing \\"`,
		`"short \u000"`:   `"short \u000"`,
	} {
		if got := string(jsonbSafe([]byte(in))); got != want {
			t.Errorf("jsonbSafe(%s) = %s, want %s", in, got, want)
		}
	}
	// What json.Marshal makes of a NUL comes out jsonb-safe and still valid.
	data := marshal(map[string]string{"r": "x\x00y"})
	var back map[string]string
	if err := json.Unmarshal(data, &back); err != nil || back["r"] != "x\ufffdy" {
		t.Errorf("marshal(NUL) = %s, %v", data, err)
	}
}

func TestMarshalFailureIsRecorded(t *testing.T) {
	data := marshal(map[string]any{"f": func() {}})
	var back map[string]string
	if err := json.Unmarshal(data, &back); err != nil || back["marshal_error"] == "" {
		t.Errorf("marshal(func) = %s, %v", data, err)
	}
}

func TestErrKind(t *testing.T) {
	if errKind(nil) != "" || errKind(errors.New("x")) != "other" ||
		errKind(fmt.Errorf("w: %w", &llms.ProviderError{Kind: llms.ErrorKindTimeout})) != "timeout" {
		t.Error("errKind")
	}
}

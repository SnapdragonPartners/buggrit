package review

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/telemetry"
)

// recorded is a run's telemetry in memory.
type recorded struct {
	events []*telemetry.Event
	mu     sync.Mutex
}

func (r *recorded) recorder() *telemetry.Recorder {
	return telemetry.NewRecorder(func(e *telemetry.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		e.Seq = len(r.events) + 1
		r.events = append(r.events, e)
	})
}

func (r *recorded) of(kind, session string) []*telemetry.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*telemetry.Event
	for _, e := range r.events {
		if e.Kind == kind && e.Session == session {
			out = append(out, e)
		}
	}
	return out
}

func decode[T any](t *testing.T, e *telemetry.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Every request a stage sent rebuilds from its telemetry, through the
// nudge and the wrap-up, and each tool call names the call that asked for
// it.
func checkRebuilds(t *testing.T, rec *recorded, session string, fake *testllm.FakeChatClient) {
	t.Helper()
	rec.mu.Lock()
	events := append([]*telemetry.Event(nil), rec.events...)
	rec.mu.Unlock()
	for i, req := range fake.Calls() {
		got, err := telemetry.Rebuild(events, session, i+1)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(req.Messages) {
			t.Fatalf("call %d: rebuilt %d messages, sent %d", i+1, len(got), len(req.Messages))
		}
		for j := range req.Messages {
			want, _ := json.Marshal(req.Messages[j])
			if string(got[j]) != string(want) {
				t.Errorf("call %d message %d:\n got %s\nwant %s", i+1, j, got[j], want)
			}
		}
	}
}

func TestTelemetryRecordsAStage(t *testing.T) {
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.ToolCallResponse("1", "read_file", `{"path": "internal/cache/lru.go"}`),
		testllm.TextResponse("Looks fine."),
		testllm.ToolCallResponse("2", "submit_findings", submitOne),
	}}
	rec := &recorded{}
	in := findInput(t, fake)
	in.Session = rec.recorder().Session("find")
	res, err := Find(context.Background(), in)
	if err != nil || !res.Complete {
		t.Fatalf("result = %+v, %v", res, err)
	}
	checkRebuilds(t, rec, "find", fake)

	calls := rec.of(telemetry.KindCall, "find")
	if len(calls) != 3 {
		t.Fatalf("%d call events, want 3", len(calls))
	}
	phases := []string{}
	for _, c := range calls {
		phases = append(phases, decode[struct{ Phase string }](t, c).Phase)
	}
	if strings.Join(phases, ",") != "investigate,investigate,nudge" {
		t.Errorf("phases = %v", phases)
	}
	tools := rec.of(telemetry.KindTool, "find")
	if len(tools) != 2 || tools[0].Call != 1 || tools[1].Call != 3 {
		t.Fatalf("tool events: %d, calls %v", len(tools), tools)
	}
	if tr := decode[struct{ Name, Result string }](t, tools[0]); tr.Name != "read_file" || !strings.Contains(tr.Result, "same map bug") {
		t.Errorf("tool record = %+v", tr)
	}
	st := rec.of(telemetry.KindStage, "find")
	if len(st) != 1 {
		t.Fatalf("%d stage events", len(st))
	}
	if sr := decode[stageRecord](t, st[0]); !sr.Submitted || sr.Iterations != res.Iterations {
		t.Errorf("stage record = %+v", sr)
	}
	op := rec.of(telemetry.KindOpening, "find")
	if len(op) != 1 || op[0].Call != 0 {
		t.Fatalf("opening events: %v", op)
	}
	pk := decode[packing](t, op[0])
	if len(pk.Diff) != 2 || pk.Diff[0].Path != "internal/cache/cache.go" || pk.Diff[0].Truncated || len(pk.Deferred) != 0 ||
		len(pk.Instructions) != 1 || pk.Instructions[0].Path != "CLAUDE.md" || pk.OtherThreads != 1 || pk.ReviewInstructions == 0 ||
		pk.Bytes != len(fake.Calls()[0].Messages[0].Content[0].Text)-len(budgetOf(fake)) {
		t.Errorf("packing = %+v", pk)
	}
}

// budgetOf is the time budget the stage appends to its opening message.
func budgetOf(fake *testllm.FakeChatClient) string {
	text := fake.Calls()[0].Messages[0].Content[0].Text
	_, after, _ := strings.Cut(text, "Investigate as needed, then call submit_findings once with every finding.\n")
	return after
}

func TestTelemetryRecordsTheWrapUp(t *testing.T) {
	fake := &testllm.FakeChatClient{Func: func(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
		if req.Purpose == "buggrit.find.wrapup" {
			return testllm.ToolCallResponse("w", "submit_findings", submitOne), nil
		}
		if len(req.Messages) == 1 {
			return testllm.ToolCallResponse("1", "read_file", `{"path": "internal/cache/lru.go"}`), nil
		}
		<-ctx.Done()
		return llms.ChatResponse{}, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rec := &recorded{}
	in := findInput(t, fake)
	in.WrapUp = 1900 * time.Millisecond
	in.Session = rec.recorder().Session("find")
	if _, err := Find(ctx, in); err != nil {
		t.Fatal(err)
	}
	checkRebuilds(t, rec, "find", fake)
	calls := rec.of(telemetry.KindCall, "find")
	last := decode[struct {
		ToolChoice llms.ToolChoice `json:"tool_choice"`
		Phase      string
		Purpose    string
		Error      string
		Tools      []string
	}](t, calls[len(calls)-1])
	if last.Phase != "wrapup" || last.Purpose != "buggrit.find.wrapup" || len(last.Tools) != 1 || last.ToolChoice.Name != "submit_findings" {
		t.Errorf("wrap-up call = %+v", last)
	}
	if mid := decode[struct{ Error string }](t, calls[1]); mid.Error == "" {
		t.Error("the call the deadline cut off should record its error")
	}
}

func TestTelemetryOpeningDefersALargeDiff(t *testing.T) {
	in := findInput(t, &testllm.FakeChatClient{})
	h := &in.Diff.Files[0].Hunks[0]
	for i := range maxPromptDiffBytes / 20 {
		h.Lines = append(h.Lines, github.DiffLine{Kind: github.LineAdded, New: 100 + i, Text: "padding padding pad"})
	}
	second := in.Diff.Files[0]
	second.NewPath, second.OldPath = "internal/cache/cache2.go", "internal/cache/cache2.go"
	in.Diff.Files = append(in.Diff.Files, second)
	pk := &packing{}
	msg := openingMessage(in, pk)
	if len(pk.Diff) != 2 || pk.Diff[0].Path != "internal/cache/cache.go" || !pk.Diff[0].Truncated || len(pk.Deferred) != 1 ||
		pk.Deferred[0] != "internal/cache/cache2.go" || pk.Bytes != len(msg) {
		t.Errorf("packing = %+v", pk)
	}
}

// A pass records its inputs and what it decided, and its finder's session.
func TestTelemetryRecordsAPass(t *testing.T) {
	_, gh, ref := engineSetup(t)
	model := submitting(`{"summary": "One bug.", "coverage": "complete", "findings": [` + validFinding + `]}`)
	rec := &recorded{}
	opts := engineOptions(model)
	opts.Telemetry = rec.recorder()
	res, err := Review(context.Background(), ref, testWorkspace(), gh, opts)
	if err != nil {
		t.Fatal(err)
	}
	passes := rec.of(telemetry.KindPass, "")
	if len(passes) != 1 {
		t.Fatalf("%d pass events", len(passes))
	}
	pr := decode[struct {
		Mode   string
		PR     Context
		Diff   struct{ Files []json.RawMessage }
		Pass   int
		Finder bool
	}](t, passes[0])
	if pr.Mode != "full" || pr.Pass != 1 || !pr.Finder || len(pr.Diff.Files) != 3 || pr.PR.HeadSHA != engineHead {
		t.Errorf("pass record = %+v", pr)
	}
	results := rec.of(telemetry.KindResult, "")
	if len(results) != 1 {
		t.Fatalf("%d result events", len(results))
	}
	rr := decode[resultRecord](t, results[0])
	if !rr.Done || len(rr.Submitted) != 1 || len(rr.Kept) != 1 || rr.Kept[0].ID != res.Findings[0].ID || rr.Summary != "One bug." {
		t.Errorf("result record = %+v", rr)
	}
	if n := len(rec.of(telemetry.KindCall, "find")); n != 1 {
		t.Errorf("%d find calls recorded", n)
	}
}

// A pass that fails still records why.
func TestTelemetryRecordsAFailedPass(t *testing.T) {
	_, gh, ref := engineSetup(t)
	ref.HeadSHA = engineBase // the head moved
	rec := &recorded{}
	opts := engineOptions(submitting(`{"coverage": "complete", "findings": []}`))
	opts.Telemetry = rec.recorder()
	if _, err := Review(context.Background(), ref, testWorkspace(), gh, opts); err == nil {
		t.Fatal("want the head-moved error")
	}
	results := rec.of(telemetry.KindResult, "")
	if len(results) != 1 || decode[resultRecord](t, results[0]).Error == "" {
		t.Errorf("result events: %v", results)
	}
}

// Records of empty inputs encode their lists as [], never null, which the
// documented queries' array functions can't take (docs/telemetry.md).
func TestTelemetryRecordsHaveNoNullLists(t *testing.T) {
	cfg := config.Defaults()
	cfg.Review.InstructionFiles = nil
	p := &pass{cfg: &cfg, res: &Result{PR: &github.PullRequest{}}, diff: &github.Diff{}}
	for name, rec := range map[string]any{
		"packing":      (&packing{}).record(),
		"pass":         p.record(nil),
		"pass, finder": p.record(&FindInput{}),
		"result":       newResultRecord(&Result{Find: &FindResult{}}, nil),
		"failed":       newResultRecord(nil, errors.New("boom")),
		"stage":        stageRecord{},
	} {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "null") {
			t.Errorf("%s: %s", name, data)
		}
	}
}

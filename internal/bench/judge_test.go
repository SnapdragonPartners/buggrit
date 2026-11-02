package bench

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

func testBenchmark(t *testing.T) *Benchmark {
	t.Helper()
	b, _, err := ParseBenchmark(benchmarkJSON(nil))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testRun() *RunResult {
	return &RunResult{ID: "run-1", Findings: []Finding{
		{Path: "a.go", Line: 2, Severity: "P1", Title: "first", Problem: "p1"},
		{Path: "c.go", Line: 9, Severity: "P3", Title: "second", Class: "same thing", Instances: []Instance{{Path: "b.md", Line: 4}}},
		{Path: "z.go", Line: 1, Severity: "P2", Title: "third"},
	}}
}

const validJudgment = `{
  "defects": [
    {"id": "d2", "verdict": "partial", "finding": 2, "reasoning": "points there, reads it wrong"},
    {"id": "d1", "verdict": "found", "finding": 1, "reasoning": "same bug"}
  ],
  "extras": [{"finding": 3, "verdict": "false", "reasoning": "not a defect"}]
}`

func TestJudgeScores(t *testing.T) {
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("c1", judgeTool, validJudgment)}}
	s, err := (&Judge{Chat: fake}).Score(context.Background(), testBenchmark(t), testRun())
	if err != nil {
		t.Fatal(err)
	}
	if s.RunID != "run-1" || len(s.Defects) != 2 || s.Defects[0].Defect != "d1" || s.Defects[1].Verdict != Partial || s.Defects[1].Finding != 2 {
		t.Fatalf("defects in the benchmark's order: %+v", s.Defects)
	}
	if len(s.Extras) != 1 || s.Extras[0].Verdict != False {
		t.Fatalf("extras: %+v", s.Extras)
	}
	if got := s.Recall(); got != 0.75 {
		t.Errorf("Recall = %v, want 0.75", got)
	}
	// An override counts instead of the draft.
	s.Defects[1].Override = Missed
	if got := s.Recall(); got != 0.5 {
		t.Errorf("Recall with an override = %v, want 0.5", got)
	}

	calls := fake.Calls()
	if len(calls) != 1 || len(calls[0].Tools) != 1 || calls[0].Tools[0].Name != judgeTool {
		t.Fatalf("calls: %+v", calls)
	}
	prompt := calls[0].Messages[0].Content[0].Text
	for _, want := range []string{"**d1** (raised by copilot, at a.go:1-3)", "overlapping location (a hint, not a rule): 1", "### Finding 2: second", "A class finding (same thing), also at: b.md:4", "Problem: p1"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

func TestJudgeRetriesAnInvalidJudgment(t *testing.T) {
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{
		testllm.TextResponse("I think d1 is found."),
		testllm.ToolCallResponse("c1", judgeTool, `{"defects": [{"id": "d1", "verdict": "found", "finding": 7, "reasoning": "x"}], "extras": [{"finding": 1, "verdict": "maybe", "reasoning": "x"}]}`),
		testllm.ToolCallResponse("c2", judgeTool, validJudgment),
	}}
	s, err := (&Judge{Chat: fake}).Score(context.Background(), testBenchmark(t), testRun())
	if err != nil || len(s.Defects) != 2 {
		t.Fatalf("Score = %+v, %v", s, err)
	}
	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("%d calls, want 3", len(calls))
	}
	last := calls[2].Messages[len(calls[2].Messages)-1]
	res := last.Content[0].ToolResult
	if res == nil || !res.IsError || res.ToolCallID != "c1" {
		t.Fatalf("the problems go back as the tool's error result: %+v", last)
	}
	for _, want := range []string{"names finding 7, which doesn't exist", `defect "d2" has no verdict`, `verdict "maybe"`, "finding 2 matches no defect"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("problems lack %q:\n%s", want, res.Content)
		}
	}
}

func TestJudgeGivesUp(t *testing.T) {
	fake := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.TextResponse("no")}}
	_, err := (&Judge{Chat: fake, MaxAttempts: 2}).Score(context.Background(), testBenchmark(t), testRun())
	if err == nil || !strings.Contains(err.Error(), "no valid judgment in 2 calls") || len(fake.Calls()) != 2 {
		t.Fatalf("err = %v after %d calls", err, len(fake.Calls()))
	}
	fake = &testllm.FakeChatClient{Err: errors.New("boom")}
	if _, err := (&Judge{Chat: fake}).Score(context.Background(), testBenchmark(t), testRun()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a model error: %v", err)
	}
}

func TestJudgeNoFindingsNeedsNoCall(t *testing.T) {
	fake := &testllm.FakeChatClient{Err: errors.New("must not be called")}
	s, err := (&Judge{Chat: fake}).Score(context.Background(), testBenchmark(t), &RunResult{ID: "r"})
	if err != nil || len(s.Defects) != 2 || s.Defects[0].Verdict != Missed || s.Recall() != 0 {
		t.Fatalf("Score = %+v, %v", s, err)
	}
}

func TestParseJudgmentRejects(t *testing.T) {
	b := testBenchmark(t)
	cases := []struct{ params, want string }{
		{`not json`, "valid JSON"},
		{`{"defects": [{"id": "d9", "verdict": "found", "finding": 1}], "extras": []}`, `"d9" isn't in the benchmark`},
		{`{"defects": [{"id": "d1", "verdict": "missed"}, {"id": "d1", "verdict": "missed"}, {"id": "d2", "verdict": "missed"}], "extras": [{"finding": 1, "verdict": "real"}]}`, "judged twice"},
		{`{"defects": [{"id": "d1", "verdict": "found", "finding": 1}, {"id": "d2", "verdict": "missed"}], "extras": [{"finding": 1, "verdict": "real"}]}`, "matches a defect, so it isn't an extra"},
		{`{"defects": [{"id": "d1", "verdict": "sorta", "finding": 1}, {"id": "d2", "verdict": "missed"}], "extras": []}`, `verdict "sorta"`},
	}
	for _, c := range cases {
		if _, problems := parseJudgment([]byte(c.params), b, 1); !strings.Contains(strings.Join(problems, "; "), c.want) {
			t.Errorf("%s: problems = %v, want %q", c.params, problems, c.want)
		}
	}
}

func TestOverlaps(t *testing.T) {
	d := &Defect{Locations: []Location{{Path: "a.go", Start: 10, End: 20}, {Path: "doc.md"}}}
	cases := []struct {
		f    Finding
		want bool
	}{
		{Finding{Path: "a.go", Line: 15}, true},
		{Finding{Path: "a.go", Line: 25}, false},
		{Finding{Path: "a.go", StartLine: 5, Line: 10}, true},
		{Finding{Path: "a.go", StartLine: 21, Line: 30}, false},
		{Finding{Path: "a.go"}, true}, // a file-level finding
		{Finding{Path: "doc.md", Line: 900}, true},
		{Finding{Path: "b.go", Line: 15}, false},
		{Finding{Path: "b.go", Line: 1, Instances: []Instance{{Path: "a.go", Line: 12}}}, true},
	}
	for _, c := range cases {
		if got := d.Overlaps(&c.f); got != c.want {
			t.Errorf("Overlaps(%s) = %v, want %v", c.f.Where(), got, c.want)
		}
	}
}

package bench

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestApplyResult(t *testing.T) {
	payload := `{
	  "submitted": [
	    {"path": "a.go", "line": 3, "severity": "P2", "title": "dup", "problem": "first copy"},
	    {"path": "a.go", "line": 9, "severity": "P2", "title": "dup", "problem": "second copy"},
	    {"path": "b.go", "line": 1, "severity": "P3", "title": "dropped", "problem": "below threshold"}
	  ],
	  "kept": [
	    {"id": "f-1", "path": "a.go", "line": 4, "severity": "P1", "title": "dup"},
	    {"id": "f-2", "path": "a.go", "line": 9, "severity": "P2", "title": "dup"}
	  ],
	  "done": false, "reason": "the pass deadline passed", "elapsed_ms": 61000
	}`
	var r RunResult
	if err := r.ApplyResult([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if !r.HasResult || r.Done || r.Reason != "the pass deadline passed" || r.Wall() != 61*time.Second {
		t.Fatalf("result = %+v", r)
	}
	// Each kept finding takes the next unused submitted one's text, with
	// the anchor and severity reconcile settled on.
	if len(r.Findings) != 2 || r.Findings[0].Problem != "first copy" || r.Findings[0].Line != 4 || r.Findings[0].Severity != "P1" ||
		r.Findings[1].Problem != "second copy" {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

func TestAssignArms(t *testing.T) {
	runs := []RunResult{{Label: "exp/a"}, {Label: "other/a"}, {Label: "exp/b/c"}}
	AssignArms(&Experiment{Name: "exp"}, runs)
	if runs[0].Arm != "a" || runs[1].Arm != "" || runs[2].Arm != "b/c" {
		t.Fatalf("arms = %q %q %q", runs[0].Arm, runs[1].Arm, runs[2].Arm)
	}
}

func identity(model, effort, commit string) Identity {
	var id Identity
	_ = json.Unmarshal([]byte(`{
	  "models": {"find": {"model": "`+model+`", "reasoning_effort": "`+effort+`"}},
	  "mph": {"stages": {"find": {"prompt_hash": "sha256:aaaabbbbcccc", "tools_hash": "sha256:1111222233"}}, "commit": "`+commit+`"}
	}`), &id)
	return id
}

func reportInput(t *testing.T) *ReportInput {
	t.Helper()
	b := testBenchmark(t)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ended := at.Add(40 * time.Minute)
	e := &Experiment{
		Name: "trial", Runs: 2, Benchmark: "bench/b1.json",
		Judge: &config.Model{Provider: "anthropic", Model: "claude-x", ReasoningEffort: "high"},
		Arms:  []Arm{{Name: "deepseek", Ref: "main"}, {Name: "glm", Ref: "exp/glm"}},
	}
	runs := []RunResult{
		{
			ID: "r-glm", Label: "trial/glm", Arm: "glm", Group: "g2", StartedAt: at, Identity: identity("glm-5.3", "medium", "abcdef1234"),
			HasResult: true, Done: true, ElapsedMS: 600000, Turns: 10, ToolCalls: 25,
			Tokens:   Tokens{Input: 1000, Output: 200, Reasoning: 50, Cached: 400},
			Findings: []Finding{{Path: "z.go", Line: 1, Severity: "P1", Title: "a | pipe", Problem: "wrong"}},
		},
		{
			ID: "r-ds-2", Label: "trial/deepseek", Arm: "deepseek", Group: "g1", StartedAt: at.Add(time.Minute), Identity: identity("deepseek", "medium", "1234567890"),
			HasResult: true, Reason: "the pass deadline passed", ElapsedMS: 1800000, Turns: 30, ToolCalls: 30,
		},
		{
			ID: "r-ds-1", Label: "trial/deepseek", Arm: "deepseek", Group: "g1", StartedAt: at, EndedAt: &ended, Identity: identity("deepseek", "medium", "1234567890"),
			HasResult: true, Done: true, ElapsedMS: 1200000, Turns: 20, ToolCalls: 40,
			Tokens: Tokens{Input: 5000, Output: 700},
			Findings: []Finding{
				{Path: "a.go", Line: 2, Severity: "P2", Title: "first", Problem: "it breaks"},
				{Path: "q.go", StartLine: 3, Line: 5, Severity: "P3", Title: "extra one", Class: "c", Instances: []Instance{{Path: "q.go", Line: 9}}},
			},
		},
	}
	scores := map[string]*RunScore{
		"r-ds-1": {RunID: "r-ds-1",
			Defects: []DefectScore{
				{Defect: "d1", Verdict: Found, Finding: 1, Reasoning: "same bug"},
				{Defect: "d2", Verdict: Partial, Finding: 1, Reasoning: "close", Override: Missed, Note: "different file"},
			},
			Extras: []ExtraScore{{Finding: 2, Verdict: Real, Reasoning: "plausible"}},
		},
		"r-ds-2": {RunID: "r-ds-2", Defects: []DefectScore{{Defect: "d1", Verdict: Missed}, {Defect: "d2", Verdict: Missed}}},
		"r-glm": {RunID: "r-glm",
			Defects: []DefectScore{{Defect: "d1", Verdict: Missed}, {Defect: "d2", Verdict: Missed}},
			Extras:  []ExtraScore{{Finding: 1, Verdict: False, Reasoning: "misread", Override: Real, Note: "it's real"}},
		},
	}
	return &ReportInput{
		Experiment: &ExperimentRecord{Experiment: e, Benchmark: b, BenchmarkHash: "sha256:0123456789abcdef", Replay: &Replay{Repo: "o/r", PR: 11}},
		Runs:       runs, Scores: scores, RunURL: "https://github.com/o/r/actions/runs/42/attempts/1",
	}
}

func TestReportGolden(t *testing.T) {
	got := Report(reportInput(t))
	golden := "testdata/report.golden.md"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("report differs from %s (go test -run TestReportGolden -update):\n%s", golden, got)
	}
}

// Without a benchmark the report has no score columns or sections.
func TestReportWithoutBenchmark(t *testing.T) {
	in := reportInput(t)
	in.Experiment.Benchmark, in.Experiment.Experiment.Judge, in.Scores = nil, nil, nil
	in.Experiment.Experiment.Target = &Target{Repo: "o/r", Base: shaA, Head: shaB, Title: "T"}
	got := Report(in)
	for _, absent := range []string{"Recall", "Known defects", "Extra findings", "Judge:"} {
		if strings.Contains(got, absent) {
			t.Errorf("report has %q:\n%s", absent, got)
		}
	}
	if !strings.Contains(got, "### Findings by run") || !strings.Contains(got, "o/r `6d36cd0...e619281`") {
		t.Errorf("report:\n%s", got)
	}
}

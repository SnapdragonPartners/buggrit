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
	// A P3 dropped below the threshold shares the kept P1's path and title.
	payload := `{
	  "submitted": [
	    {"path": "a.go", "line": 3, "severity": "P3", "title": "dup", "problem": "the dropped one"},
	    {"path": "a.go", "line": 9, "severity": "P1", "title": "dup", "problem": "the kept one"},
	    {"path": "b.go", "line": 1, "severity": "P2", "title": "other", "problem": "kept too"}
	  ],
	  "kept": [
	    {"id": "f-1", "path": "a.go", "line": 9, "severity": "P1", "title": "dup", "submitted": 2},
	    {"id": "f-2", "path": "b.go", "line": 0, "severity": "P2", "title": "other", "submitted": 3}
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
	if len(r.Findings) != 2 || r.Findings[0].Problem != "the kept one" || r.Findings[1].Problem != "kept too" || r.Findings[1].Line != 0 {
		t.Fatalf("findings = %+v", r.Findings)
	}

	// A run recorded before kept findings named their submission. A
	// submission dropped for low confidence shares path, title and
	// severity with the kept one; the line tells them apart.
	var old RunResult
	legacy := `{"submitted": [
	    {"path": "a.go", "line": 3, "severity": "P1", "confidence": "low", "title": "dup", "problem": "the dropped one"},
	    {"path": "a.go", "line": 9, "severity": "P1", "confidence": "high", "title": "dup", "problem": "the kept one"},
	    {"path": "d.go", "line": 0, "severity": "P2", "confidence": "low", "title": "file", "problem": "dropped file-level"},
	    {"path": "d.go", "line": 0, "severity": "P2", "confidence": "high", "title": "file", "problem": "kept file-level"}],
	  "kept": [
	    {"path": "a.go", "line": 9, "severity": "P1", "title": "dup"},
	    {"path": "c.go", "line": 2, "severity": "P2", "title": "unmatched"},
	    {"path": "d.go", "line": 0, "severity": "P2", "title": "file"}]}`
	if err := old.ApplyResult([]byte(legacy)); err != nil {
		t.Fatal(err)
	}
	if len(old.Findings) != 3 || old.Findings[0].Problem != "the kept one" || old.Findings[1].Problem != "" || old.Findings[1].Title != "unmatched" {
		t.Fatalf("legacy findings = %+v", old.Findings)
	}
	// Two file-level submissions match equally: no text rather than a guess.
	if f := old.Findings[2]; f.Problem != "" || f.Title != "file" {
		t.Fatalf("an ambiguous match took a submission's text: %+v", f)
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
			HasResult: true, Complete: false, Done: true, ElapsedMS: 600000, Turns: 10, ToolCalls: 25,
			Tokens:   Tokens{Input: 1000, Output: 200, Reasoning: 50, Cached: 400},
			Findings: []Finding{{Path: "z.go", Line: 1, Severity: "P1", Title: "a | pipe", Problem: "wrong"}},
		},
		{
			ID: "r-ds-2", Label: "trial/deepseek", Arm: "deepseek", Group: "g1", StartedAt: at.Add(time.Minute), Identity: identity("deepseek", "medium", "1234567890"),
			HasResult: true, Complete: true, Reason: "the pass deadline passed", ElapsedMS: 1800000, Turns: 30, ToolCalls: 30,
		},
		{
			ID: "r-ds-1", Label: "trial/deepseek", Arm: "deepseek", Group: "g1", StartedAt: at, EndedAt: &ended, Identity: identity("deepseek", "medium", "1234567890"),
			HasResult: true, Complete: true, Done: true, ElapsedMS: 1200000, Turns: 20, ToolCalls: 40,
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
		// glm's ref has no recorded commit, as in an experiment recorded
		// before schema v4, so it shows the ref alone.
		Experiment: &ExperimentRecord{Experiment: e, Benchmark: b, BenchmarkHash: "sha256:0123456789abcdef", Replay: &Replay{Repo: "o/r", PR: 11},
			Commits: map[string]string{"main": "1234567890" + strings.Repeat("0", 30)}},
		Runs: runs, Scores: scores, RunURL: "https://github.com/o/r/actions/runs/42/attempts/1",
	}
}

// A run whose telemetry wasn't all written has its counts marked as lower
// bounds, and they're left out of the group's averages.
func TestReportIncompleteTelemetry(t *testing.T) {
	got := Report(reportInput(t))
	for _, want := range []string{"| glm 1 | 10m00s | ≥10 |", "| ≥1000 / 200 / 50 / 400 | complete | incomplete |", "| glm | `g2` | 1 | 10m00s | – | – | – | – / – | 1 of 1 | 0 of 1 | 1.0 |"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
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
	// Scores without their benchmark (a row whose benchmark is gone) are
	// left out rather than read against nothing.
	in.Experiment.Benchmark, in.Experiment.Experiment.Judge = nil, nil
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

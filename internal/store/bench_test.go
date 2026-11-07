package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/bench"
	"github.com/SnapdragonPartners/buggrit/internal/telemetry"
)

// A review works with schema v2, which has no score tables; the harness
// needs v4, which records the commits an experiment's arms built, and
// refuses v3 as well as v2.
func TestOpenBenchNeedsV4(t *testing.T) {
	if BenchSchemaVersion != 4 {
		t.Fatalf("BenchSchemaVersion = %d, want 4 (experiment.commits)", BenchSchemaVersion)
	}
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	ctx := context.Background()
	for _, found := range []int{3, 2} {
		exec(t, dsn, fmt.Sprintf("DELETE FROM buggrit.schema_version WHERE version > %d", found))
		var se *SchemaError
		if _, err := OpenBench(ctx, dsn); !errors.As(err, &se) || se.Required != 4 || se.Found != found {
			t.Fatalf("OpenBench against v%d: %v", found, err)
		}
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("a review against v2: %v", err)
	}
	_ = s.Close()
}

// fakeRun writes a finished dry run of PR 11 with a finder session: two
// calls, three tool calls, a stage end and a result.
func fakeRun(t *testing.T, s *Store, id, label, workflowRun, attempt string, findings int) {
	t.Helper()
	ctx := context.Background()
	sink := s.Telemetry()
	meta := map[string]any{
		"models": map[string]any{"find": map[string]string{"model": "deepseek", "reasoning_effort": "medium"}},
		"mph":    map[string]any{"commit": "0123456789", "stages": map[string]any{"find": map[string]string{"prompt_hash": "sha256:aa", "tools_hash": "sha256:bb"}}},
		"limits": map[string]any{"pass_deadline": "55m"}, "review": map[string]any{"min_severity": "P3"},
		"workflow_run": map[string]string{"id": workflowRun, "attempt": attempt, "event": "workflow_dispatch"},
	}
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := sink.StartRun(ctx, &telemetry.Run{ID: id, StartedAt: at, Repo: "o/r", PR: 11, HeadSHA: "abc", DryRun: true, Label: label, Meta: meta}); err != nil {
		t.Fatal(err)
	}
	kept := make([]map[string]any, 0, findings)
	submitted := make([]map[string]any, 0, findings)
	for i := range findings {
		title := string(rune('a' + i))
		kept = append(kept, map[string]any{"id": "f", "path": "a.go", "line": i + 1, "severity": "P2", "title": title})
		submitted = append(submitted, map[string]any{"path": "a.go", "line": i + 1, "severity": "P2", "title": title, "problem": "p " + title})
	}
	ev := func(seq int, kind, session string, call int, payload any) *telemetry.Event {
		data, _ := json.Marshal(payload)
		return &telemetry.Event{Seq: seq, At: at, Kind: kind, Session: session, Call: call, Payload: data}
	}
	usage := func(in, out int) map[string]any {
		return map[string]any{"response": map[string]any{"usage": map[string]int{"InputTokens": in, "OutputTokens": out, "ReasoningTokens": 5, "CacheReadTokens": 7}}}
	}
	events := []*telemetry.Event{
		ev(1, telemetry.KindCall, "find", 1, usage(100, 10)),
		ev(2, telemetry.KindTool, "find", 1, map[string]string{"name": "read_file"}),
		ev(3, telemetry.KindTool, "find", 1, map[string]string{"name": "grep"}),
		ev(4, telemetry.KindCall, "find", 2, usage(200, 20)),
		ev(5, telemetry.KindTool, "find", 2, map[string]string{"name": "submit_findings"}),
		ev(6, telemetry.KindCall, "verify", 1, usage(9999, 9999)), // not the finder's
		ev(7, telemetry.KindStage, "find", 2, map[string]any{"reason": "terminal", "submitted": true}),
		ev(8, telemetry.KindResult, "", 0, map[string]any{"done": true, "elapsed_ms": 90000, "kept": kept, "submitted": submitted}),
	}
	if err := sink.WriteEvents(ctx, id, events); err != nil {
		t.Fatal(err)
	}
	if err := sink.FinishRun(ctx, id, &telemetry.End{EndedAt: at.Add(2 * time.Minute), Outcome: map[string]any{"exit_code": 0, "status": "complete"}, Emitted: 8, Written: 8, Complete: true}); err != nil {
		t.Fatal(err)
	}
}

func TestBenchStoreRoundTrip(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	ctx := context.Background()
	s, err := OpenBench(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	b := s.Bench()

	const (
		run1 = "11111111-1111-4111-8111-111111111111"
		run2 = "22222222-2222-4222-8222-222222222222"
		run3 = "33333333-3333-4333-8333-333333333333"
	)
	fakeRun(t, s, run1, "exp/a", "42", "1", 2)
	fakeRun(t, s, run2, "exp/b", "42", "1", 0)
	fakeRun(t, s, run3, "exp/a", "42", "2", 1) // another attempt

	bm := &bench.Benchmark{Name: "b1", Target: bench.Target{Repo: "o/r", Base: "base", Head: "head", Title: "T"},
		Defects: []bench.Defect{{ID: "d1", Source: "copilot", Description: "one", Locations: []bench.Location{{Path: "a.go"}}}}}
	for range 2 {
		if serr := b.SaveBenchmark(ctx, "sha256:abc", bm); serr != nil {
			t.Fatal(serr)
		}
	}
	spec := json.RawMessage(`{"name": "exp", "benchmark": "bench/b1.json", "judge": {"provider": "openai-compatible", "base_url": "https://x.example/v1", "model": "j"}, "arms": [{"name": "a"}, {"name": "b"}]}`)
	ex, err := bench.ParseExperiment(spec)
	if err != nil {
		t.Fatal(err)
	}
	w := bench.WorkflowRun{ID: 42, Attempt: 1}
	rec := &bench.ExperimentRecord{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Experiment: ex, Spec: spec, Workflow: w, BenchmarkHash: "sha256:abc", Benchmark: bm,
		Replay:  &bench.Replay{Repo: "o/r", PR: 11, Base: "bench/exp/42-1/base", Head: "bench/exp/42-1/head"},
		Commits: map[string]string{"main": strings.Repeat("1", 40)},
	}
	id, err := b.SaveExperiment(ctx, rec)
	if err != nil || id != rec.ID {
		t.Fatalf("SaveExperiment = %q, %v", id, err)
	}
	// Saving the same workflow run again keeps the first.
	rec2 := *rec
	rec2.ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if id, err = b.SaveExperiment(ctx, &rec2); err != nil || id != rec.ID {
		t.Fatalf("SaveExperiment again = %q, %v", id, err)
	}
	got, err := b.LoadExperiment(ctx, w)
	if err != nil || got.ID != rec.ID || got.Replay.PR != 11 || got.Benchmark == nil || got.Benchmark.Defects[0].ID != "d1" || got.Experiment.Arms[1].Name != "b" || got.Commits["main"] != strings.Repeat("1", 40) {
		t.Fatalf("LoadExperiment = %+v, %v", got, err)
	}
	// An experiment recorded while "issue" was a field still loads, its
	// spec as recorded.
	old := json.RawMessage(`{"name": "old", "benchmark": "bench/b1.json", "judge": {"provider": "openai-compatible", "base_url": "https://x.example/v1", "model": "j"}, "Issue": 19, "arms": [{"name": "a"}]}`)
	oldW := bench.WorkflowRun{ID: 41, Attempt: 1}
	oldRec := &bench.ExperimentRecord{
		ID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Experiment: &bench.Experiment{Name: "old"}, Spec: old, Workflow: oldW,
		BenchmarkHash: "sha256:abc", Replay: &bench.Replay{Repo: "o/r", PR: 10},
	}
	if _, serr := b.SaveExperiment(ctx, oldRec); serr != nil {
		t.Fatal(serr)
	}
	// An experiment recorded without commits (before v4) loads with none.
	if got, lerr := b.LoadExperiment(ctx, oldW); lerr != nil || got.Experiment.Name != "old" || !strings.Contains(string(got.Spec), `"Issue": 19`) || len(got.Commits) != 0 {
		t.Fatalf("LoadExperiment of a recorded issue = %+v, %v", got, lerr)
	}
	if _, lerr := b.LoadExperiment(ctx, bench.WorkflowRun{ID: 7, Attempt: 1}); !errors.Is(lerr, bench.ErrNoExperiment) {
		t.Fatalf("LoadExperiment of an unknown run: %v", lerr)
	}

	runs, err := b.Runs(ctx, w, rec.Replay)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != run1 || runs[1].ID != run2 {
		t.Fatalf("runs = %+v", runs)
	}
	r := &runs[0]
	if r.Turns != 2 || r.ToolCalls != 3 || r.Tokens != (bench.Tokens{Input: 300, Output: 30, Reasoning: 10, Cached: 14}) ||
		!r.Complete || !r.Done || !r.HasResult || r.ElapsedMS != 90000 || r.StageReason != "terminal" || !r.Submitted ||
		r.Status != "complete" || r.EndedAt == nil || len(r.Findings) != 2 || r.Findings[1].Problem != "p b" || len(r.Group) != 8 {
		t.Fatalf("run = %+v", r)
	}
	if runs[1].Group != r.Group {
		t.Error("runs with the same identity are in different groups")
	}
	if model, effort := r.Identity.FindModel(); model != "deepseek" || effort != "medium" || r.Identity.MPH.Commit != "0123456789" {
		t.Errorf("identity = %s %s %+v", model, effort, r.Identity.MPH)
	}

	score := &bench.RunScore{RunID: run1,
		Defects: []bench.DefectScore{{Defect: "d1", Verdict: bench.Partial, Finding: 1, Reasoning: "close"}},
		Extras:  []bench.ExtraScore{{Finding: 2, Verdict: bench.Real, Reasoning: "plausible"}},
	}
	if err = b.SaveScore(ctx, rec.ID, score, r.Findings); err != nil {
		t.Fatal(err)
	}
	// A person overrides the draft, as docs/benchmarking.md says.
	exec(t, dsn,
		"UPDATE buggrit.score SET override_verdict = 'found', override_note = 'it is the same bug' WHERE run_id = '"+run1+"' AND defect = 'd1'",
		"UPDATE buggrit.extra_finding SET override_verdict = 'false' WHERE run_id = '"+run1+"' AND finding = 2")
	// Scoring again changes nothing, so the override stays, and none of
	// the second judgment is saved: its extra would count finding 1 as
	// both a match and an extra.
	again := &bench.RunScore{RunID: run1, Defects: []bench.DefectScore{{Defect: "d1", Verdict: bench.Missed}},
		Extras: []bench.ExtraScore{{Finding: 1, Verdict: bench.False}}}
	if err = b.SaveScore(ctx, rec.ID, again, r.Findings); err != nil {
		t.Fatal(err)
	}
	scores, err := b.Scores(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	s1 := scores[run1]
	if s1 == nil || len(s1.Defects) != 1 || len(s1.Extras) != 1 || scores[run2] != nil {
		t.Fatalf("scores = %+v", scores)
	}
	d := s1.Defects[0]
	if d.Verdict != bench.Partial || d.Override != bench.Found || d.Note != "it is the same bug" || d.Finding != 1 || d.Effective() != bench.Found {
		t.Errorf("defect score = %+v", d)
	}
	if x := s1.Extras[0]; x.Verdict != bench.Real || x.Effective() != bench.False {
		t.Errorf("extra = %+v", x)
	}
	if serr := b.SaveScore(ctx, rec.ID, &bench.RunScore{RunID: run2, Extras: []bench.ExtraScore{{Finding: 1, Verdict: bench.Real}}}, nil); serr == nil {
		t.Error("an extra for a finding the run doesn't have was saved")
	}

	// docs/telemetry.md's queries over scores run, with a value in every
	// column.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	checkDocQueries(t, db, run1, true)
}

// CheckWrites passes for the runtime role's grants (docs/database.md) and
// names what a login can't write.
func TestBenchCheckWrites(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	ctx := context.Background()
	role := fmt.Sprintf("buggrit_bench_%d", time.Now().UnixNano())
	exec(t, dsn,
		"CREATE ROLE "+role+" LOGIN",
		"GRANT USAGE ON SCHEMA buggrit, bucket TO "+role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA buggrit, bucket TO "+role)
	t.Cleanup(func() {
		exec(t, dsn, "DROP OWNED BY "+role, "DROP ROLE "+role)
	})
	u, _ := url.Parse(dsn)
	u.User = url.User(role)
	s, err := OpenBench(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	err = s.Bench().CheckWrites(ctx)
	if err == nil || !strings.Contains(err.Error(), "can't write telemetry") || !strings.Contains(err.Error(), "or scores") {
		t.Fatalf("CheckWrites as a read-only login = %v", err)
	}
	exec(t, dsn, "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA buggrit, bucket TO "+role)
	if err := s.Bench().CheckWrites(ctx); err != nil {
		t.Fatalf("CheckWrites with the runtime grants = %v", err)
	}
}

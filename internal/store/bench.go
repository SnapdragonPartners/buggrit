package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/SnapdragonPartners/buggrit/internal/bench"
	storedb "github.com/SnapdragonPartners/buggrit/internal/store/db"
)

// Bench returns the benchmark harness's view of the database (design §12,
// "Benchmarks"): the runs an experiment recorded, and its scores. Open the
// store with OpenBench, which checks for the harness's schema version: the
// score tables (v3) and each experiment's recorded commits (v4).
func (s *Store) Bench() bench.Store { return benchStore{s: s, q: storedb.New(s.pool)} }

type benchStore struct {
	s *Store
	q *storedb.Queries
}

// ErrNoExperiment is LoadExperiment's error for a workflow run with no
// experiment recorded.
var ErrNoExperiment = errors.New("no experiment recorded for that workflow run")

func (b benchStore) SaveBenchmark(ctx context.Context, hash string, bm *bench.Benchmark) error {
	content, err := json.Marshal(bm)
	if err != nil {
		return fmt.Errorf("encode the benchmark: %w", err)
	}
	if err := b.q.SaveBenchmark(ctx, storedb.SaveBenchmarkParams{
		Hash: hash, Name: bm.Name, Repo: bm.Repo, BaseSha: bm.Base, HeadSha: bm.Head, Content: content,
	}); err != nil {
		return fmt.Errorf("save benchmark %s: %w", bm.Name, err)
	}
	return nil
}

func (b benchStore) SaveExperiment(ctx context.Context, e *bench.ExperimentRecord) (string, error) {
	id, err := uuid(e.ID)
	if err != nil {
		return "", err
	}
	replay, err := json.Marshal(e.Replay)
	if err != nil {
		return "", fmt.Errorf("encode the replay: %w", err)
	}
	c := e.Commits
	if c == nil {
		c = map[string]string{}
	}
	commits, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode the commits: %w", err)
	}
	got, err := b.q.SaveExperiment(ctx, storedb.SaveExperimentParams{
		ID: id, Name: e.Experiment.Name, WorkflowRun: e.Workflow.ID, Attempt: int32(e.Workflow.Attempt), //nolint:gosec // an attempt number fits
		Spec: e.Spec, Benchmark: pgtype.Text{String: e.BenchmarkHash, Valid: e.BenchmarkHash != ""}, Replay: replay,
		Commits: commits,
	})
	if err != nil {
		return "", fmt.Errorf("save experiment %s: %w", e.Experiment.Name, err)
	}
	return got.String(), nil
}

func (b benchStore) LoadExperiment(ctx context.Context, w bench.WorkflowRun) (*bench.ExperimentRecord, error) {
	row, err := b.q.LoadExperiment(ctx, storedb.LoadExperimentParams{WorkflowRun: w.ID, Attempt: int32(w.Attempt)}) //nolint:gosec // an attempt number fits
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("workflow run %d attempt %d: %w", w.ID, w.Attempt, ErrNoExperiment)
	case err != nil:
		return nil, fmt.Errorf("load the experiment of workflow run %d: %w", w.ID, err)
	}
	ex, err := bench.ParseRecordedExperiment(row.Spec)
	if err != nil {
		return nil, fmt.Errorf("the stored experiment: %w", err)
	}
	rec := &bench.ExperimentRecord{ID: row.ID.String(), Experiment: ex, Spec: row.Spec, Workflow: w, Replay: &bench.Replay{}}
	if err := json.Unmarshal(row.Replay, rec.Replay); err != nil {
		return nil, fmt.Errorf("the stored replay: %w", err)
	}
	if err := json.Unmarshal(row.Commits, &rec.Commits); err != nil {
		return nil, fmt.Errorf("the stored commits: %w", err)
	}
	if row.Benchmark.Valid {
		rec.BenchmarkHash = row.Benchmark.String
		rec.Benchmark = &bench.Benchmark{}
		if err := json.Unmarshal(row.BenchmarkContent, rec.Benchmark); err != nil {
			return nil, fmt.Errorf("the stored benchmark: %w", err)
		}
	}
	return rec, nil
}

func (b benchStore) Runs(ctx context.Context, w bench.WorkflowRun, r *bench.Replay) ([]bench.RunResult, error) {
	rows, err := b.q.BenchRuns(ctx, storedb.BenchRunsParams{
		Repo: r.Repo, Pr: int32(r.PR), //nolint:gosec // a PR number fits
		WorkflowRun: strconv.FormatInt(w.ID, 10), Attempt: strconv.Itoa(w.Attempt),
	})
	if err != nil {
		return nil, fmt.Errorf("read the experiment's runs: %w", err)
	}
	out := make([]bench.RunResult, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		rr := bench.RunResult{
			ID: row.ID.String(), Label: row.Label, StartedAt: row.StartedAt.Time, Complete: row.Complete,
			Group: row.IdentityGroup, Turns: int(row.Calls), ToolCalls: int(row.Tools),
			Tokens: bench.Tokens{Input: row.InputTokens, Output: row.OutputTokens, Reasoning: row.ReasoningTokens, Cached: row.CachedTokens},
		}
		if row.EndedAt.Valid {
			t := row.EndedAt.Time
			rr.EndedAt = &t
		}
		if err := json.Unmarshal(row.Identity, &rr.Identity); err != nil {
			return nil, fmt.Errorf("run %s: identity: %w", rr.ID, err)
		}
		if len(row.Outcome) > 0 {
			var o struct {
				Status   string `json:"status"`
				Error    string `json:"error"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal(row.Outcome, &o); err != nil {
				return nil, fmt.Errorf("run %s: outcome: %w", rr.ID, err)
			}
			rr.Status, rr.Error, rr.ExitCode = o.Status, o.Error, o.ExitCode
		}
		if len(row.Stage) > 0 {
			var st struct {
				Reason    string `json:"reason"`
				Submitted bool   `json:"submitted"`
			}
			if err := json.Unmarshal(row.Stage, &st); err != nil {
				return nil, fmt.Errorf("run %s: stage: %w", rr.ID, err)
			}
			rr.StageReason, rr.Submitted = st.Reason, st.Submitted
		}
		if len(row.Result) > 0 {
			if err := rr.ApplyResult(row.Result); err != nil {
				return nil, fmt.Errorf("run %s: result: %w", rr.ID, err)
			}
		}
		out = append(out, rr)
	}
	return out, nil
}

// CheckWrites checks this login may write what a benchmark records: the
// review runs' telemetry and the harness's scores.
func (b benchStore) CheckWrites(ctx context.Context) error {
	p, err := b.q.BenchPrivileges(ctx)
	if err != nil {
		return fmt.Errorf("read this login's privileges: %w", err)
	}
	var missing []string
	if !p.Telemetry.Bool {
		missing = append(missing, "telemetry (buggrit.run, buggrit.event)")
	}
	if !p.Scores.Bool {
		missing = append(missing, "scores (buggrit.benchmark, experiment, score, extra_finding)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("this login can't write %s; grant buggrit_runtime as docs/database.md says", strings.Join(missing, " or "))
	}
	return nil
}

// SaveScore writes a run's judgment in one transaction.
func (b benchStore) SaveScore(ctx context.Context, experimentID string, s *bench.RunScore, findings []bench.Finding) error {
	exp, err := uuid(experimentID)
	if err != nil {
		return err
	}
	run, err := uuid(s.RunID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, b.s.pool, func(tx pgx.Tx) error { //nolint:wrapcheck // fn's errors name the run; Begin and Commit's are pgx's, as is
		q := b.q.WithTx(tx)
		// A run is judged once: with the run locked, a judgment already
		// saved (by another report) stands, and this one's rows, defects
		// and extras alike, are dropped together.
		if err := q.LockRun(ctx, run); err != nil {
			return fmt.Errorf("lock run %s: %w", s.RunID, err)
		}
		scored, err := q.RunScored(ctx, run)
		if err != nil {
			return fmt.Errorf("read the score of run %s: %w", s.RunID, err)
		}
		if scored {
			return nil
		}
		for i := range s.Defects {
			d := &s.Defects[i]
			if err := q.InsertScore(ctx, storedb.InsertScoreParams{
				RunID: run, ExperimentID: exp, Defect: d.Defect, Verdict: d.Verdict, Reasoning: d.Reasoning,
				Finding: pgtype.Int4{Int32: int32(d.Finding), Valid: d.Finding > 0}, //nolint:gosec // a finding number fits
			}); err != nil {
				return fmt.Errorf("save the score of run %s: %w", s.RunID, err)
			}
		}
		for i := range s.Extras {
			x := &s.Extras[i]
			if x.Finding < 1 || x.Finding > len(findings) {
				return fmt.Errorf("run %s: extra finding %d doesn't exist", s.RunID, x.Finding)
			}
			f := &findings[x.Finding-1]
			if err := q.InsertExtraFinding(ctx, storedb.InsertExtraFindingParams{
				RunID: run, ExperimentID: exp, Finding: int32(x.Finding), //nolint:gosec // a finding number fits
				Path: f.Path, Line: int32(f.Line), Title: f.Title, Verdict: x.Verdict, Reasoning: x.Reasoning, //nolint:gosec // a line number fits
			}); err != nil {
				return fmt.Errorf("save the extra findings of run %s: %w", s.RunID, err)
			}
		}
		return nil
	})
}

func (b benchStore) Scores(ctx context.Context, experimentID string) (map[string]*bench.RunScore, error) {
	exp, err := uuid(experimentID)
	if err != nil {
		return nil, err
	}
	scores, err := b.q.Scores(ctx, exp)
	if err != nil {
		return nil, fmt.Errorf("read scores: %w", err)
	}
	extras, err := b.q.ExtraFindings(ctx, exp)
	if err != nil {
		return nil, fmt.Errorf("read extra findings: %w", err)
	}
	out := map[string]*bench.RunScore{}
	get := func(id pgtype.UUID) *bench.RunScore {
		k := id.String()
		if out[k] == nil {
			out[k] = &bench.RunScore{RunID: k}
		}
		return out[k]
	}
	for i := range scores {
		r := &scores[i]
		s := get(r.RunID)
		s.Defects = append(s.Defects, bench.DefectScore{
			Defect: r.Defect, Verdict: r.Verdict, Finding: int(r.Finding), Reasoning: r.Reasoning,
			Override: r.OverrideVerdict, Note: r.OverrideNote,
		})
	}
	for i := range extras {
		r := &extras[i]
		s := get(r.RunID)
		s.Extras = append(s.Extras, bench.ExtraScore{
			Finding: int(r.Finding), Verdict: r.Verdict, Reasoning: r.Reasoning,
			Override: r.OverrideVerdict, Note: r.OverrideNote,
		})
	}
	return out, nil
}

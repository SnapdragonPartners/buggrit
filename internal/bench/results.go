package bench

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// RunResult is one review run of an experiment, as its telemetry recorded
// it: the run, its finder session's calls and tool calls, how the stage
// ended and what the pass kept.
type RunResult struct {
	StartedAt time.Time
	EndedAt   *time.Time
	// Identity is the run's model/prompt/harness identity with its limits
	// and review settings, and Group its short hash: runs are averaged only
	// within a group (docs/telemetry.md, "Identity").
	Identity Identity
	ID       string
	Label    string
	// Arm is the label's arm part; "" for a label from another experiment.
	Arm   string
	Group string
	// Status is the run's status line, Error its error; Reason why the pass
	// stopped short, when it did.
	Status string
	Error  string
	Reason string
	// StageReason is how the finder stage ended (the toolloop outcome);
	// Submitted whether the model submitted.
	StageReason string
	// Findings are the findings the pass kept, which a live run would have
	// posted.
	Findings []Finding
	// ElapsedMS is the pass's wall time.
	ElapsedMS int64
	Turns     int
	ToolCalls int
	Tokens    Tokens
	ExitCode  int
	// Complete means telemetry wrote every event; Done that the pass
	// finished.
	Complete  bool
	Done      bool
	Submitted bool
	// HasResult is false for a run cut off before its result was recorded.
	HasResult bool
}

// Tokens sums the finder's model calls.
type Tokens struct {
	Input     int64
	Output    int64
	Reasoning int64
	Cached    int64
}

// Identity is what a run ran (docs/telemetry.md, "Identity").
type Identity struct {
	Models json.RawMessage `json:"models"`
	MPH    struct {
		Stages map[string]struct {
			PromptHash string `json:"prompt_hash"`
			ToolsHash  string `json:"tools_hash"`
		} `json:"stages"`
		Commit   string `json:"commit"`
		Modified bool   `json:"modified"`
	} `json:"mph"`
	Limits json.RawMessage `json:"limits"`
	Review json.RawMessage `json:"review"`
}

// FindModel is the finder's provider, model and reasoning effort.
func (id *Identity) FindModel() (model, effort string) {
	var models map[string]struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	_ = json.Unmarshal(id.Models, &models) // a malformed identity shows as blank
	m := models["find"]
	return m.Model, m.ReasoningEffort
}

// Finding is one finding a run kept, with the text the finder submitted.
type Finding struct {
	Path      string     `json:"path"`
	Severity  string     `json:"severity"`
	Category  string     `json:"category,omitempty"`
	Title     string     `json:"title"`
	Problem   string     `json:"problem,omitempty"`
	Fix       string     `json:"fix,omitempty"`
	Class     string     `json:"class,omitempty"`
	Instances []Instance `json:"instances,omitempty"`
	StartLine int        `json:"start_line,omitempty"`
	Line      int        `json:"line"`
}

// Instance is one place a class finding names.
type Instance struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// Blocking reports a P0 or P1.
func (f *Finding) Blocking() bool { return f.Severity == "P0" || f.Severity == "P1" }

// resultPayload is the part of a result event's payload the harness reads.
type resultPayload struct {
	Reason    string    `json:"reason"`
	Error     string    `json:"error"`
	Submitted []Finding `json:"submitted"`
	Kept      []struct {
		Path     string `json:"path"`
		Severity string `json:"severity"`
		Title    string `json:"title"`
		Line     int    `json:"line"`
	} `json:"kept"`
	ElapsedMS int64 `json:"elapsed_ms"`
	Done      bool  `json:"done"`
}

// ApplyResult fills r from a result event's payload: the kept findings,
// each with the text of the submitted finding it came from (by path and
// title), and how the pass ended.
func (r *RunResult) ApplyResult(payload []byte) error {
	var p resultPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return err //nolint:wrapcheck // the caller names the run
	}
	r.HasResult, r.Done, r.Reason, r.ElapsedMS = true, p.Done, p.Reason, p.ElapsedMS
	if p.Error != "" && r.Error == "" {
		r.Error = p.Error
	}
	used := make([]bool, len(p.Submitted))
	r.Findings = make([]Finding, 0, len(p.Kept))
	for i := range p.Kept {
		k := &p.Kept[i]
		f := Finding{Path: k.Path, Line: k.Line, Severity: k.Severity, Title: k.Title}
		for j := range p.Submitted {
			s := &p.Submitted[j]
			if !used[j] && s.Title == k.Title && s.Path == k.Path {
				used[j] = true
				f = *s
				f.Line, f.Severity = k.Line, k.Severity
				break
			}
		}
		r.Findings = append(r.Findings, f)
	}
	return nil
}

// Wall is the pass's wall time, or the run's when the pass recorded none.
func (r *RunResult) Wall() time.Duration {
	if r.ElapsedMS > 0 {
		return time.Duration(r.ElapsedMS) * time.Millisecond
	}
	if r.EndedAt != nil {
		return r.EndedAt.Sub(r.StartedAt)
	}
	return 0
}

// Store is the database the harness reads runs from and keeps scores in;
// internal/store implements it.
type Store interface {
	// SaveBenchmark records a benchmark by its content hash, once.
	SaveBenchmark(ctx context.Context, hash string, b *Benchmark) error
	// SaveExperiment records an experiment run and returns its ID. Saving
	// the same workflow run again returns the existing ID and changes
	// nothing.
	SaveExperiment(ctx context.Context, e *ExperimentRecord) (string, error)
	// LoadExperiment reads back an experiment run, with its benchmark.
	LoadExperiment(ctx context.Context, w WorkflowRun) (*ExperimentRecord, error)
	// Runs returns the dry runs of the replay that ran in the workflow
	// run, in label then start order.
	Runs(ctx context.Context, w WorkflowRun, r *Replay) ([]RunResult, error)
	// SaveScore records a run's judgment; findings are the run's, which
	// extra findings refer to by number. A run already scored keeps its
	// scores, and any overrides on them.
	SaveScore(ctx context.Context, experimentID string, s *RunScore, findings []Finding) error
	// Scores returns the experiment's scores by run ID, with overrides.
	Scores(ctx context.Context, experimentID string) (map[string]*RunScore, error)
}

// ExperimentRecord is an experiment run as the database keeps it.
type ExperimentRecord struct {
	Experiment *Experiment
	// Benchmark and BenchmarkHash are nil and "" without a benchmark.
	Benchmark     *Benchmark
	Replay        *Replay
	ID            string
	BenchmarkHash string
	// Spec is the experiment file as given.
	Spec     json.RawMessage
	Workflow WorkflowRun
}

// AssignArms sets each run's arm from its label, "<experiment>/<arm>".
func AssignArms(e *Experiment, runs []RunResult) {
	for i := range runs {
		if arm, ok := strings.CutPrefix(runs[i].Label, e.Name+"/"); ok {
			runs[i].Arm = arm
		}
	}
}

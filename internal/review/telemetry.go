package review

import (
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// passRecord is a pass's inputs, as telemetry records them before the
// stages run: enough to replay the pass or interpret its requests
// (docs/telemetry.md).
type passRecord struct {
	Diff             *github.Diff  `json:"diff"`
	PR               Context       `json:"pr"`
	Mode             string        `json:"mode"`
	Scope            string        `json:"scope,omitempty"`
	Since            string        `json:"since,omitempty"`
	Instructions     string        `json:"instructions,omitempty"`
	InstructionFiles []string      `json:"instruction_files"`
	OtherThreads     []ThreadNote  `json:"other_threads"`
	Checks           []checkRecord `json:"checks"`
	Pass             int           `json:"pass"`
	Finder           bool          `json:"finder"`
}

type checkRecord struct {
	ID       string             `json:"id"`
	State    state.FindingState `json:"state"`
	Severity string             `json:"severity"`
	Why      string             `json:"why"`
}

func (p *pass) record(fin *FindInput) *passRecord {
	res := p.res
	rec := &passRecord{
		Diff: p.diff, PR: p.context(), Mode: "full", Scope: res.Scope, Pass: res.Pass, Finder: fin != nil,
		Instructions: p.cfg.Review.Instructions, InstructionFiles: p.cfg.Review.InstructionFiles,
	}
	if res.ReconcileOnly {
		rec.Mode = "reconcile"
	}
	if fin != nil {
		rec.OtherThreads = fin.OtherThreads
		if fin.Incremental != nil {
			rec.Since = fin.Incremental.Since
		}
	}
	for i := range res.Checks {
		c := &res.Checks[i]
		rec.Checks = append(rec.Checks, checkRecord{ID: c.Tracked.ID, State: c.Tracked.State, Severity: c.Tracked.Severity, Why: c.Why})
	}
	return rec
}

// resultRecord is what a pass decided: the findings the finder submitted
// and those reconcile kept, the verdicts and the transitions. Publishing's
// outcome is the front end's to record.
type resultRecord struct {
	Verdicts       map[string]*state.Verdict `json:"verdicts,omitempty"`
	Error          string                    `json:"error,omitempty"`
	SkipReason     string                    `json:"skip_reason,omitempty"`
	Reason         string                    `json:"reason,omitempty"`
	Summary        string                    `json:"summary,omitempty"`
	Submitted      []Finding                 `json:"submitted"`
	Kept           []keptRecord              `json:"kept"`
	Transitions    []transitionRecord        `json:"transitions"`
	Unverified     []string                  `json:"unverified"`
	Duplicates     int                       `json:"duplicates"`
	BelowThreshold int                       `json:"below_threshold"`
	ElapsedMS      int64                     `json:"elapsed_ms"`
	Skipped        bool                      `json:"skipped"`
	Complete       bool                      `json:"complete"`
	Done           bool                      `json:"done"`
}

type keptRecord struct {
	ID       string `json:"id"`
	FP       string `json:"fp"`
	Path     string `json:"path"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Line     int    `json:"line"`
}

type transitionRecord struct {
	ID     string     `json:"id"`
	Kind   state.Kind `json:"kind,omitempty"`
	Reason string     `json:"reason,omitempty"`
	Want   state.Want `json:"want,omitempty"`
}

func newResultRecord(res *Result, err error) *resultRecord {
	rec := &resultRecord{}
	if err != nil {
		rec.Error = err.Error()
	}
	if res == nil {
		return rec
	}
	rec.Skipped, rec.SkipReason, rec.Reason, rec.Summary = res.Skipped, res.SkipReason, res.Reason, res.Summary
	rec.Complete, rec.Done, rec.ElapsedMS = res.Complete, res.Done(), res.Elapsed.Milliseconds()
	rec.Duplicates, rec.BelowThreshold, rec.Unverified = res.Duplicates, res.BelowThreshold, res.Unverified
	if res.Find != nil {
		rec.Submitted = res.Find.Findings
	}
	if res.Verify != nil {
		rec.Verdicts = res.Verify.Verdicts
	}
	for i := range res.Findings {
		c := &res.Findings[i]
		rec.Kept = append(rec.Kept, keptRecord{ID: c.ID, FP: c.FP, Path: c.Anchor.Path, Line: c.Anchor.Line, Severity: c.Severity, Title: c.Title})
	}
	for i := range res.Transitions {
		t := &res.Transitions[i]
		rec.Transitions = append(rec.Transitions, transitionRecord{ID: t.Tracked.ID, Kind: t.Kind, Want: t.Want, Reason: t.Reason})
	}
	return rec
}

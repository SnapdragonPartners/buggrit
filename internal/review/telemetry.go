package review

import (
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

// passRecord is a pass's inputs, as telemetry records them before the
// stages run: enough to replay the pass or interpret its requests
// (docs/telemetry.md).
type passRecord struct {
	Diff             *github.Diff   `json:"diff"`
	PR               prRecord       `json:"pr"`
	Mode             string         `json:"mode"`
	Scope            string         `json:"scope,omitempty"`
	Since            string         `json:"since,omitempty"`
	Instructions     string         `json:"instructions,omitempty"`
	InstructionFiles []string       `json:"instruction_files"`
	OtherThreads     []threadRecord `json:"other_threads"`
	Checks           []checkRecord  `json:"checks"`
	Pass             int            `json:"pass"`
	Finder           bool           `json:"finder"`
}

// prRecord and threadRecord are Context and ThreadNote with the keys
// docs/telemetry.md names, as verdictRecord is state.Verdict.
type prRecord struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	HeadSHA string `json:"head_sha"`
	BaseRef string `json:"base_ref"`
	Number  int    `json:"number"`
}

type threadRecord struct {
	Path     string `json:"path"`
	Author   string `json:"author"`
	Body     string `json:"body"`
	Line     int    `json:"line"`
	Resolved bool   `json:"resolved"`
}

type checkRecord struct {
	ID       string             `json:"id"`
	State    state.FindingState `json:"state"`
	Severity string             `json:"severity"`
	Why      string             `json:"why"`
}

// Every list in a record encodes as [], never null: a JSON null isn't SQL's,
// so it would break array functions in the documented queries
// (docs/telemetry.md).
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// record returns the packing with every list non-nil.
func (pk *packing) record() *packing {
	out := *pk
	out.Instructions, out.InstructionsOmitted = nonNil(pk.Instructions), nonNil(pk.InstructionsOmitted)
	out.Diff, out.Deferred = nonNil(pk.Diff), nonNil(pk.Deferred)
	return &out
}

func (p *pass) record(fin *FindInput) *passRecord {
	res := p.res
	diff := github.Diff{}
	if p.diff != nil {
		diff = *p.diff
	}
	diff.Files = nonNil(diff.Files)
	rec := &passRecord{
		Diff: &diff, PR: prRecord(p.context()), Mode: "full", Scope: res.Scope, Pass: res.Pass, Finder: fin != nil,
		Instructions: p.cfg.Review.Instructions, InstructionFiles: nonNil(p.cfg.Review.InstructionFiles),
		OtherThreads: []threadRecord{}, Checks: []checkRecord{},
	}
	if res.ReconcileOnly {
		rec.Mode = "reconcile"
	}
	if fin != nil {
		for i := range fin.OtherThreads {
			rec.OtherThreads = append(rec.OtherThreads, threadRecord(fin.OtherThreads[i]))
		}
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
	Verdicts       map[string]verdictRecord `json:"verdicts,omitempty"`
	Error          string                   `json:"error,omitempty"`
	SkipReason     string                   `json:"skip_reason,omitempty"`
	Reason         string                   `json:"reason,omitempty"`
	Summary        string                   `json:"summary,omitempty"`
	Submitted      []Finding                `json:"submitted"`
	Kept           []keptRecord             `json:"kept"`
	Transitions    []transitionRecord       `json:"transitions"`
	Unverified     []string                 `json:"unverified"`
	Duplicates     int                      `json:"duplicates"`
	BelowThreshold int                      `json:"below_threshold"`
	ElapsedMS      int64                    `json:"elapsed_ms"`
	Skipped        bool                     `json:"skipped"`
	Complete       bool                     `json:"complete"`
	Done           bool                     `json:"done"`
}

type verdictRecord struct {
	State    state.FindingState `json:"state"`
	Reason   string             `json:"reason"`
	Evidence []string           `json:"evidence"`
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
	rec := &resultRecord{Submitted: []Finding{}, Kept: []keptRecord{}, Transitions: []transitionRecord{}, Unverified: []string{}}
	if err != nil {
		rec.Error = err.Error()
	}
	if res == nil {
		return rec
	}
	rec.Skipped, rec.SkipReason, rec.Reason, rec.Summary = res.Skipped, res.SkipReason, res.Reason, res.Summary
	rec.Complete, rec.Done, rec.ElapsedMS = res.Complete, res.Done(), res.Elapsed.Milliseconds()
	rec.Duplicates, rec.BelowThreshold, rec.Unverified = res.Duplicates, res.BelowThreshold, nonNil(res.Unverified)
	if res.Find != nil {
		rec.Submitted = nonNil(res.Find.Findings)
	}
	if res.Verify != nil {
		rec.Verdicts = map[string]verdictRecord{}
		for id, v := range res.Verify.Verdicts {
			if v == nil {
				continue
			}
			rec.Verdicts[id] = verdictRecord{State: v.State, Reason: v.Reason, Evidence: nonNil(v.Evidence)}
		}
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

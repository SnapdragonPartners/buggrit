package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/publish"
	"github.com/SnapdragonPartners/buggrit/internal/review"
	"github.com/SnapdragonPartners/buggrit/internal/telemetry"
)

// telemetryLabelEnv names an experiment or packing variant for the runs it's
// set on (docs/telemetry.md).
const telemetryLabelEnv = "BUGGRIT_TELEMETRY_LABEL"

// telemetryFlush bounds writing the last events when the run ends, also
// after the job was canceled.
const telemetryFlush = 30 * time.Second

// startTelemetry starts recording the run when the database is open and the
// repository hasn't turned telemetry off (design §6, "Telemetry"). It
// leaves r.rec nil otherwise, which records nothing.
func (r *reviewer) startTelemetry(db database, cfg *config.Config, pr *github.PullRequest) {
	if db == nil || !cfg.Telemetry.Enabled {
		return
	}
	run := &telemetry.Run{
		ID: newRunID(), StartedAt: time.Now(), Repo: r.repo.String(), PR: pr.Number, HeadSHA: pr.HeadSHA,
		DryRun: r.args.dryRun, Label: r.args.telemetryLabel, Meta: r.runMeta(cfg, pr),
	}
	r.writer = telemetry.NewWriter(db.Telemetry(), run, telemetry.WriterOptions{})
	r.rec = telemetry.NewRecorder(r.writer.Emit)
	say(r.stderr, "buggrit: recording telemetry as run %s\n", run.ID)
}

// runMeta is what the run was: the code, the effective models and limits,
// and where it ran. Models carry the names of their key variables, never
// the keys.
func (r *reviewer) runMeta(cfg *config.Config, pr *github.PullRequest) map[string]any {
	models := map[config.Stage]config.Model{}
	for _, s := range config.Stages() {
		models[s] = cfg.Stage(s)
	}
	meta := map[string]any{
		"buggrit":      version,
		"maestro_llms": moduleVersion("github.com/SnapdragonPartners/maestro-llms"),
		"go":           runtime.Version(),
		"base_ref":     pr.BaseRef,
		"models":       models,
		"limits":       cfg.Limits,
		"review": map[string]any{
			"min_severity": cfg.Review.MinSeverity, "min_confidence": cfg.Review.MinConfidence,
			"include": cfg.Review.Include, "exclude": cfg.Review.Exclude,
		},
		"shared_rate_limit": r.args.rateLimits != "",
		"mph":               runMPH(debug.ReadBuildInfo()),
	}
	if r.args.runID != "" {
		meta["workflow_run"] = map[string]string{"id": r.args.runID, "attempt": r.args.runAttempt, "event": r.args.eventName}
	}
	return meta
}

// mph is the prompt and harness parts of the run's model/prompt/harness
// identity; the models are meta's models (design §6, "Telemetry").
type mph struct {
	Stages map[string]review.StageIdentity `json:"stages"`
	// Commit and Modified are the build's vcs.revision and vcs.modified:
	// empty and false for a build without them, such as go run.
	Commit   string `json:"commit"`
	Modified bool   `json:"modified"`
}

// runMPH is the identity of the code running, from the build info.
func runMPH(info *debug.BuildInfo, ok bool) mph {
	id := mph{Stages: review.Identity()}
	if !ok {
		return id
	}
	for i := range info.Settings {
		switch s := &info.Settings[i]; s.Key {
		case "vcs.revision":
			id.Commit = s.Value
		case "vcs.modified":
			id.Modified = s.Value == "true"
		}
	}
	return id
}

// moduleVersion is the version of a module built into the binary, or ""
// when the build info doesn't say.
func moduleVersion(path string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, m := range info.Deps {
		if m.Path == path {
			if m.Replace != nil {
				return m.Replace.Version
			}
			return m.Version
		}
	}
	return ""
}

// newRunID is a random (version 4) UUID.
func newRunID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// runOutcome is how the run ended, for its row.
type runOutcome struct {
	Status   string `json:"status,omitempty"`
	Stopped  string `json:"stopped,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// endTelemetry writes the run's last events and ends its row. Events it
// couldn't write are a database problem: the run goes on, and the warning
// says how many were lost.
func (r *reviewer) endTelemetry(ctx context.Context, code int) {
	if r.writer == nil {
		return
	}
	r.outcome.ExitCode = code
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlush)
	defer cancel()
	rep := r.writer.Close(fctx, r.outcome)
	if p := rep.Problem(); p != "" {
		r.warnDatabase(p)
		return
	}
	say(r.stderr, "buggrit: telemetry: wrote %d events\n", rep.Written)
}

// publishRecord is what publishing did.
type publishRecord struct {
	Error    string         `json:"error,omitempty"`
	Manual   []manualRecord `json:"manual,omitempty"`
	ReviewID int64          `json:"review_id,omitempty"`
	Threads  int            `json:"threads"`
	Replies  int            `json:"replies"`
	Resolved int            `json:"resolved"`
	Reopened int            `json:"unresolved"`
	FellBack bool           `json:"fell_back,omitempty"`
	DryRun   bool           `json:"dry_run,omitempty"`
}

// manualRecord is publish.Manual with the keys docs/telemetry.md names.
type manualRecord struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Resolve bool   `json:"resolve"`
	New     bool   `json:"new"`
}

func newPublishRecord(posted *publish.Posted, err error) *publishRecord {
	rec := &publishRecord{}
	if err != nil {
		rec.Error = err.Error()
	}
	if posted != nil {
		rec.ReviewID, rec.Threads, rec.Replies = posted.ReviewID, posted.Threads, posted.Replies
		for i := range posted.Manual {
			rec.Manual = append(rec.Manual, manualRecord(posted.Manual[i]))
		}
		rec.Resolved, rec.Reopened, rec.FellBack = posted.Resolved, posted.Unresolved, posted.FellBack
	}
	return rec
}

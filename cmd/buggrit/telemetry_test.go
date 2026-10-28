package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/telemetry"
)

// withStore gives the run a database that opens, and returns it.
func (e *reviewEnv) withStore() *fakeStore {
	fs := &fakeStore{}
	e.env["BUGGRIT_DATABASE_URL"] = "postgres://runner@127.0.0.1/buggrit"
	e.deps.openStore = func(context.Context, string) (database, error) { return fs, nil }
	return fs
}

func TestReviewRecordsTelemetry(t *testing.T) {
	e := setupReview(t, submit(finding))
	e.fake.SetFile("main", config.Path, `{"version": 1, "models": {"default": {"provider": "openai-compatible",
	  "base_url": "https://llm.example/v1", "model": "glm-5.3", "api_key_env": "BUGGRIT_LLM_API_KEY"}}}`)
	e.env["BUGGRIT_LLM_API_KEY"] = "sk-not-for-the-database"
	e.env["GITHUB_RUN_ID"], e.env["GITHUB_RUN_ATTEMPT"], e.env["GITHUB_EVENT_NAME"] = "4242", "2", "pull_request"
	fs := e.withStore()
	code, out, errOut := e.run(t)
	if code != 0 || !strings.Contains(out, "1 new finding") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	sink := &fs.sink
	if sink.run == nil || sink.end == nil {
		t.Fatalf("run %+v, end %+v", sink.run, sink.end)
	}
	if sink.run.Repo != "acme/widgets" || sink.run.PR != 7 || sink.run.HeadSHA != head || sink.run.DryRun || len(sink.run.ID) != 36 {
		t.Errorf("run = %+v", sink.run)
	}
	meta, _ := json.Marshal(sink.run.Meta)
	if !strings.Contains(string(meta), `"BUGGRIT_LLM_API_KEY"`) || !strings.Contains(string(meta), `"id":"4242"`) {
		t.Errorf("meta = %s", meta)
	}
	kinds := sink.kinds()
	for _, k := range []string{telemetry.KindPass, telemetry.KindOpening, telemetry.KindMessage, telemetry.KindCall,
		telemetry.KindTool, telemetry.KindStage, telemetry.KindResult, telemetry.KindPublish} {
		if kinds[k] == 0 {
			t.Errorf("no %s event: %v", k, kinds)
		}
	}
	if !sink.end.Complete || sink.end.Emitted != len(sink.events) || sink.end.Written != len(sink.events) {
		t.Errorf("end = %+v with %d events", sink.end, len(sink.events))
	}
	outcome, _ := json.Marshal(sink.end.Outcome)
	if !strings.Contains(string(outcome), `"exit_code":0`) || !strings.Contains(string(outcome), "1 new finding") {
		t.Errorf("outcome = %s", outcome)
	}
	// Credentials stay out: the key variable's name is recorded, never its
	// value.
	all := string(meta) + string(outcome)
	for _, ev := range sink.events {
		all += string(ev.Payload)
	}
	if strings.Contains(all, "sk-not-for-the-database") || strings.Contains(all, "GITHUB_TOKEN") {
		t.Error("a credential reached the telemetry")
	}
	if !fs.closed {
		t.Error("the database wasn't closed")
	}
}

func TestReviewTelemetryOff(t *testing.T) {
	e := setupReview(t, submit(finding))
	e.fake.SetFile("main", config.Path, `{"version": 1, "telemetry": {"enabled": false}, "models": {"default": {"provider": "openai-compatible",
	  "base_url": "https://llm.example/v1", "model": "glm-5.3"}}}`)
	fs := e.withStore()
	if code, _, errOut := e.run(t); code != 0 || strings.Contains(errOut, "telemetry") {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if fs.sink.run != nil || len(fs.sink.events) != 0 {
		t.Errorf("recorded with telemetry off: %+v", fs.sink.run)
	}
}

// Telemetry that can't be written is a database problem: the review goes
// on, and the warning says how much was lost.
func TestReviewTelemetryWriteFails(t *testing.T) {
	e := setupReview(t, submit(finding))
	fs := e.withStore()
	fs.sink.writeErr = os.ErrDeadlineExceeded
	code, out, errOut := e.run(t)
	if code != 0 || !strings.Contains(out, "1 new finding") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	summary, err := os.ReadFile(e.summary)
	if err != nil || !strings.Contains(string(summary), "### Database") || !strings.Contains(string(summary), "telemetry: wrote 0 of") {
		t.Errorf("job summary = %q, %v", summary, err)
	}
	comments := e.fake.PR(7).IssueComments
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "telemetry: wrote 0 of") {
		t.Errorf("warning comments = %+v", comments)
	}
	if fs.sink.end == nil || fs.sink.end.Complete {
		t.Errorf("end = %+v", fs.sink.end)
	}
}

func TestDryRunTelemetryIsLabeled(t *testing.T) {
	e := setupReview(t, submit(finding))
	e.env[telemetryLabelEnv] = "packing-a"
	fs := e.withStore()
	if code, _, errOut := e.run(t, "--dry-run"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	if fs.sink.run == nil || !fs.sink.run.DryRun || fs.sink.run.Label != "packing-a" {
		t.Fatalf("run = %+v", fs.sink.run)
	}
	var published bool
	for _, ev := range fs.sink.events {
		if ev.Kind == telemetry.KindPublish {
			published = strings.Contains(string(ev.Payload), `"dry_run":true`)
		}
	}
	if !published {
		t.Error("no dry-run publish event")
	}
}

// modelLog logs each attempt and records it with the call its context
// names.
func TestModelLogRecordsAttempts(t *testing.T) {
	var events []*telemetry.Event
	rec := telemetry.NewRecorder(func(e *telemetry.Event) { events = append(events, e) })
	var b strings.Builder
	var obs middleware.ContextObserver = modelLog{w: &b, rec: rec}
	obs.ObserveContext(context.Background(), middleware.Event{Model: "glm-5.3", Latency: 1500 * time.Millisecond})
	if !strings.Contains(b.String(), "buggrit: model glm-5.3 1.5s") || len(events) != 1 || events[0].Kind != telemetry.KindAttempt {
		t.Errorf("log %q, events %v", b.String(), events)
	}
}

func TestNewRunID(t *testing.T) {
	a, b := newRunID(), newRunID()
	if a == b || len(a) != 36 || a[14] != '4' || !strings.Contains("89ab", string(a[19])) {
		t.Errorf("run IDs %s, %s", a, b)
	}
}

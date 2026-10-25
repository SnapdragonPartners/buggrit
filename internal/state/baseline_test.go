package state_test

import (
	"slices"
	"testing"
	"time"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/state"
)

func passReview(t *testing.T, author string, m *state.Marker) github.ReviewSummary {
	t.Helper()
	body, err := state.Append("pass body", m)
	if err != nil {
		t.Fatal(err)
	}
	return github.ReviewSummary{Author: author, Body: body, CommitSHA: m.SHA}
}

func TestReadBaseline(t *testing.T) {
	const bot = "buggrit-bot[bot]"
	reviews := []github.ReviewSummary{
		passReview(t, bot, &state.Marker{ID: "pass-1", Pass: 1, SHA: "aaa", Status: state.PassComplete}),
		{Author: "copilot-pull-request-reviewer[bot]", Body: "looks fine"},
		passReview(t, bot, &state.Marker{ID: "pass-2", Pass: 2, SHA: "bbb", Status: state.PassIncomplete}),
		// A forged marker from someone else doesn't count.
		passReview(t, "mallory", &state.Marker{ID: "pass-9", Pass: 9, SHA: "ccc", Status: state.PassComplete}),
	}
	b := state.ReadBaseline(reviews, bot)
	if b.LastPass != 2 || b.AttemptedSHA != "bbb" || b.CompletedSHA != "aaa" {
		t.Errorf("baseline = %+v", b)
	}
	if !b.CompletedAt("aaa") || b.CompletedAt("bbb") || b.CompletedAt("ccc") {
		t.Error("CompletedAt is wrong")
	}
	if empty := state.ReadBaseline(nil, bot); empty.LastPass != 0 || empty.CompletedAt("") {
		t.Errorf("empty baseline = %+v", empty)
	}
}

func at(sec int) time.Time { return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC) }

// A fixed finding's evidence baseline candidates are its fix and every
// later re-verification, newest first (§6, "Evidence baseline").
func TestFixRecords(t *testing.T) {
	const bot = "buggrit-bot[bot]"
	review := func(sec int, m *state.Marker) github.ReviewSummary {
		r := passReview(t, bot, m)
		r.SubmittedAt = at(sec)
		return r
	}
	reviews := []github.ReviewSummary{
		// Before the current fix: an earlier fix that regressed.
		review(1, &state.Marker{ID: "pass-1", Pass: 1, SHA: "a", Status: state.PassComplete, Verified: map[string][]string{"f-1": {"old.go"}}}),
		review(5, &state.Marker{ID: "pass-2", Pass: 2, SHA: "b", Status: state.PassIncomplete, Verified: map[string][]string{"f-1": {"x.go"}, "f-2": {"y.go"}}}),
		// A reconcile-only run's review records re-verifications too.
		review(7, &state.Marker{ID: "reconcile", SHA: "c", Verified: map[string][]string{"f-1": {"x.go", "z.go"}}}),
	}
	b := state.ReadBaseline(reviews, bot)
	if b.LastPass != 2 || b.AttemptedSHA != "b" {
		t.Errorf("the reconcile review counted as a pass: %+v", b)
	}
	f := &state.Tracked{ID: "f-1", State: state.StateFixed, Fix: &state.Record{At: at(3), SHA: "fix", Files: []string{"x.go"}}}
	got := b.FixRecords(f)
	var shas []string
	for _, r := range got {
		shas = append(shas, r.SHA)
	}
	if !slices.Equal(shas, []string{"c", "b", "fix"}) || !slices.Equal(got[0].Files, []string{"x.go", "z.go"}) {
		t.Errorf("FixRecords = %+v", got)
	}
	if open := (&state.Tracked{ID: "f-1", State: state.StateOpen}); b.FixRecords(open) != nil {
		t.Error("an open finding has fix records")
	}
}

func TestPassMarkerValidation(t *testing.T) {
	for _, m := range []*state.Marker{
		{ID: "pass-1", Pass: 1, SHA: "a", Status: "done"},
		{ID: "pass-1", SHA: "a", Status: state.PassComplete},
		{ID: "pass-1", Pass: 1, Status: state.PassComplete},
		{ID: "pass-1", Pass: 1, SHA: "a", Status: state.PassComplete, Verified: map[string][]string{"f-1": nil}},
		{ID: "reconcile", Verified: map[string][]string{"f-1": {"a.go"}}},
	} {
		if _, err := state.Encode(m); err == nil {
			t.Errorf("Encode(%+v) succeeded", m)
		}
	}
}

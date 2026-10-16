package state_test

import (
	"testing"

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

func TestPassMarkerValidation(t *testing.T) {
	for _, m := range []*state.Marker{
		{ID: "pass-1", Pass: 1, SHA: "a", Status: "done"},
		{ID: "pass-1", SHA: "a", Status: state.PassComplete},
		{ID: "pass-1", Pass: 1, Status: state.PassComplete},
	} {
		if _, err := state.Encode(m); err == nil {
			t.Errorf("Encode(%+v) succeeded", m)
		}
	}
}

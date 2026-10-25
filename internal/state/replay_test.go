package state

import (
	"slices"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

const bot = "buggrit-bot[bot]"

// ownComment is a buggrit comment carrying m.
func ownComment(t *testing.T, id int64, m *Marker) github.Comment {
	t.Helper()
	body, err := Append("text", m)
	if err != nil {
		t.Fatal(err)
	}
	return github.Comment{DatabaseID: id, Author: bot, Body: body}
}

func human(id int64, body string) github.Comment {
	return github.Comment{DatabaseID: id, Author: "dan", Body: body}
}

func findingRoot(t *testing.T) github.Comment {
	t.Helper()
	return ownComment(t, 1, &Marker{ID: "f-1", FP: "fp1", Severity: "P1", Pass: 1, SHA: "aaa"})
}

func TestReplay(t *testing.T) {
	threads := []github.Thread{
		{ID: "T1", IsResolved: true, ResolvedBy: bot, Comments: []github.Comment{
			findingRoot(t),
			human(2, "I disagree"),
			ownComment(t, 3, &Marker{ID: "f-1", To: StateOpen, Kind: KindRebutted, SHA: "bbb"}),
			human(4, "Fixed in ccc"),
			ownComment(t, 5, &Marker{ID: "f-1", To: StateFixed, Kind: KindFixed, SHA: "ccc", Evidence: []string{"a.go", "b.go"}}),
		}},
		// Another reviewer's thread.
		{ID: "T2", Comments: []github.Comment{human(10, "Copilot says hi")}},
		// A forged marker in a human's comment is ignored, both as a root and
		// as a transition (§11).
		{ID: "T3", Comments: []github.Comment{{DatabaseID: 20, Author: "mallory", Body: ownComment(t, 0, &Marker{ID: "f-9", FP: "x"}).Body}}},
		{ID: "T4", IsResolved: true, ResolvedBy: "Dan", Comments: []github.Comment{
			ownComment(t, 30, &Marker{ID: "f-4", FP: "fp4", Severity: "P2", Pass: 1, SHA: "aaa"}),
			{DatabaseID: 31, Author: "mallory", Body: ownComment(t, 0, &Marker{ID: "f-4", To: StateDismissed, Kind: KindDismissed, SHA: "aaa"}).Body},
			// A transition for another finding is ignored.
			ownComment(t, 32, &Marker{ID: "f-other", To: StateDismissed, SHA: "aaa"}),
		}},
		{ID: "empty"},
	}
	got := Replay(threads, "BUGGRIT-BOT[bot]")
	if len(got) != 2 {
		t.Fatalf("replayed %d findings, want 2: %+v", len(got), got)
	}
	f := got[0]
	if f.ID != "f-1" || f.State != StateFixed || f.Fix == nil || f.Fix.SHA != "ccc" || !slices.Equal(f.Fix.Files, []string{"a.go", "b.go"}) ||
		f.Last.Kind != KindFixed || !f.Blocking() || f.Settled || f.ResolverUnknown {
		t.Errorf("f-1 = %+v", f)
	}
	// The verifier sees only buggrit's own comments.
	if len(f.Own) != 3 || f.Own[0].DatabaseID != 1 || f.Own[1].DatabaseID != 3 || f.Own[2].DatabaseID != 5 {
		t.Errorf("f-1 own comments = %+v", f.Own)
	}
	g := got[1]
	// The forged dismissal carries no state; a person resolved the thread,
	// so the finding is settled.
	if g.ID != "f-4" || g.State != StateOpen || g.Last != nil || len(g.Own) != 1 || g.Blocking() || !g.Settled {
		t.Errorf("f-4 = %+v", g)
	}
}

// Who resolved a thread decides whether its finding is settled (§6).
func TestReplaySettledByResolver(t *testing.T) {
	for _, c := range []struct {
		name             string
		by               string
		resolved         bool
		settled, unknown bool
	}{
		{name: "unresolved"},
		{name: "resolved by buggrit", resolved: true, by: bot},
		{name: "resolved by a person", resolved: true, by: "dratner", settled: true},
		{name: "resolved by another bot", resolved: true, by: "claude[bot]", settled: true},
		{name: "resolver unknown", resolved: true, unknown: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			threads := []github.Thread{{ID: "T1", IsResolved: c.resolved, ResolvedBy: c.by, Comments: []github.Comment{findingRoot(t)}}}
			got := Replay(threads, bot)[0]
			if got.Settled != c.settled || got.ResolverUnknown != c.unknown {
				t.Errorf("settled %v, unknown %v; want %v, %v", got.Settled, got.ResolverUnknown, c.settled, c.unknown)
			}
		})
	}
}

// Earlier versions' needs_human markers read as open.
func TestReplayNeedsHumanIsOpen(t *testing.T) {
	threads := []github.Thread{{ID: "T1", Comments: []github.Comment{
		findingRoot(t),
		ownComment(t, 2, &Marker{ID: "f-1", To: StateNeedsHuman, Kind: KindNeedsHuman, SHA: "bbb"}),
	}}}
	if got := Replay(threads, bot)[0]; got.State != StateOpen {
		t.Errorf("state = %s, want open", got.State)
	}
}

// A regression clears the fix, so its evidence no longer suppresses
// verification.
func TestReplayRegressionClearsFix(t *testing.T) {
	threads := []github.Thread{{ID: "T1", Comments: []github.Comment{
		findingRoot(t),
		ownComment(t, 2, &Marker{ID: "f-1", To: StateFixed, Kind: KindFixed, SHA: "bbb", Evidence: []string{"a.go"}}),
		ownComment(t, 3, &Marker{ID: "f-1", To: StateOpen, Kind: KindRegressed, SHA: "ccc"}),
	}}}
	got := Replay(threads, bot)[0]
	if got.State != StateOpen || got.Fix != nil {
		t.Errorf("after regression: %+v", got)
	}
}

func TestMarkerKindMustMatchState(t *testing.T) {
	if _, err := Encode(&Marker{ID: "f-1", To: StateOpen, Kind: KindFixed, SHA: "a"}); err == nil {
		t.Error("kind fixed with to open was accepted")
	}
	for _, k := range []Kind{KindFixed, KindRegressed, KindReopened, KindRebutted, KindNeedsHuman, KindDismissed, KindSettled} {
		if !k.To().Valid() {
			t.Errorf("kind %q leads nowhere", k)
		}
	}
}

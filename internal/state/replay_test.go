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
		{ID: "T1", Comments: []github.Comment{
			findingRoot(t),
			human(2, "I disagree"),
			ownComment(t, 3, &Marker{ID: "f-1", To: StateOpen, Kind: KindRebutted, SHA: "bbb", Seen: 2}),
			human(4, "Fixed in ccc"),
			ownComment(t, 5, &Marker{ID: "f-1", To: StateFixed, Kind: KindFixed, SHA: "ccc", Seen: 4, Evidence: []string{"a.go", "b.go"}}),
		}},
		// Another reviewer's thread.
		{ID: "T2", Comments: []github.Comment{human(10, "Copilot says hi")}},
		// A forged marker in a human's comment is ignored, both as a root and
		// as a transition (§11).
		{ID: "T3", Comments: []github.Comment{{DatabaseID: 20, Author: "mallory", Body: ownComment(t, 0, &Marker{ID: "f-9", FP: "x"}).Body}}},
		{ID: "T4", IsResolved: true, Comments: []github.Comment{
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
	if f.ID != "f-1" || f.State != StateFixed || f.FixSHA != "ccc" || !slices.Equal(f.Evidence, []string{"a.go", "b.go"}) ||
		f.Rebuttals != 1 || f.Seen != 4 || len(f.Pending) != 0 || f.Last.Kind != KindFixed || !f.Blocking() {
		t.Errorf("f-1 = %+v", f)
	}
	g := got[1]
	// The forged dismissal is a human comment like any other.
	if g.ID != "f-4" || g.State != StateOpen || g.Last != nil || len(g.Pending) != 1 || g.Pending[0].DatabaseID != 31 || g.Blocking() {
		t.Errorf("f-4 = %+v", g)
	}
}

// A reply that lands after the verifier read the thread, but before
// buggrit's transition was posted, stays unhandled.
func TestReplaySeenBoundsHandledReplies(t *testing.T) {
	threads := []github.Thread{{ID: "T1", Comments: []github.Comment{
		findingRoot(t),
		human(2, "first"),
		human(3, "second, while buggrit was thinking"),
		ownComment(t, 4, &Marker{ID: "f-1", To: StateOpen, Kind: KindRebutted, SHA: "bbb", Seen: 2}),
	}}}
	got := Replay(threads, bot)
	if len(got[0].Pending) != 1 || got[0].Pending[0].DatabaseID != 3 {
		t.Errorf("pending = %+v", got[0].Pending)
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
	if got.State != StateOpen || got.FixSHA != "" || got.Evidence != nil {
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

package state

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
)

func TestMarkerRoundTrip(t *testing.T) {
	finding := &Marker{ID: "f-7f3a2c", FP: "abc", Severity: "P1", Pass: 2, SHA: "abc123"}
	body, err := Append("**P1 · correctness** — nil map\n\n", finding)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(body, `<!-- buggrit:v1 {"id":"f-7f3a2c","fp":"abc","sev":"P1","sha":"abc123","pass":2} -->`) {
		t.Errorf("body = %q", body)
	}
	got, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != finding.ID || got.Pass != 2 || got.IsTransition() {
		t.Errorf("parsed = %+v", got)
	}

	tr := &Marker{ID: "f-7f3a2c", To: StateFixed, SHA: "def456", Evidence: []string{"a.go", "b.go"}}
	body, err = Append("Fixed in def456.", tr)
	if err != nil {
		t.Fatal(err)
	}
	got, err = Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsTransition() || got.To != StateFixed || !slices.Equal(got.Evidence, tr.Evidence) {
		t.Errorf("parsed transition = %+v", got)
	}
}

func TestMarkerCannotCloseEarly(t *testing.T) {
	m := &Marker{ID: "f-1", FP: "--> <script>"}
	line, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(line, "-->") != 1 || !strings.HasSuffix(line, "-->") {
		t.Errorf("payload can end the comment early: %s", line)
	}
	got, err := Parse(line)
	if err != nil || got.FP != m.FP {
		t.Errorf("round trip = %+v, %v", got, err)
	}
}

func TestParseLastMarkerWins(t *testing.T) {
	body := "quoting: <!-- buggrit:v1 {\"id\":\"f-old\"} -->\n\nreply\n<!-- buggrit:v1 {\"id\":\"f-new\",\"to\":\"open\"} -->"
	m, err := Parse(body)
	if err != nil || m.ID != "f-new" {
		t.Errorf("got %+v, %v", m, err)
	}
}

func TestParseRejects(t *testing.T) {
	tests := map[string]string{
		"none":               "just a comment",
		"unterminated":       `<!-- buggrit:v1 {"id":"f-1"}`,
		"bad json":           `<!-- buggrit:v1 {"id": -->`,
		"no id":              `<!-- buggrit:v1 {"sev":"P1"} -->`,
		"unknown state":      `<!-- buggrit:v1 {"id":"f-1","to":"closed"} -->`,
		"fixed, no evidence": `<!-- buggrit:v1 {"id":"f-1","to":"fixed","sha":"x"} -->`,
		"other version":      `<!-- buggrit:v2 {"id":"f-1"} -->`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if m, err := Parse(body); err == nil {
				t.Errorf("parsed %+v", m)
			}
		})
	}
	if _, err := Parse("x"); !errors.Is(err, ErrNoMarker) {
		t.Errorf("no marker error = %v", err)
	}
	if _, err := Encode(&Marker{ID: "f-1", To: StateFixed}); err == nil {
		t.Error("encoded a fixed transition without evidence")
	}
}

func TestParseIgnoresUnknownFields(t *testing.T) {
	m, err := Parse(`<!-- buggrit:v1 {"id":"f-1","future":true} -->`)
	if err != nil || m.ID != "f-1" {
		t.Errorf("got %+v, %v", m, err)
	}
}

func TestOwnMarker(t *testing.T) {
	body, err := Append("finding", &Marker{ID: "f-1", Severity: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, author, body string
		own                bool
	}{
		{"buggrit", "buggrit-bot[bot]", body, true},
		{"case-insensitive login", "Buggrit-Bot[bot]", body, true},
		{"forged by a human", "dratner", body, false},
		{"shared actions bot", "github-actions[bot]", body, false},
		{"buggrit without marker", "buggrit-bot[bot]", "hello", false},
		{"ghost author", "", body, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, own := OwnMarker(&github.Comment{Author: tt.author, Body: tt.body}, "buggrit-bot[bot]")
			if own != tt.own {
				t.Errorf("own = %v, want %v", own, tt.own)
			}
		})
	}
}

// End to end through the fake: buggrit's marker counts, the same marker text
// pasted by a human doesn't (design §11 "a forged marker").
func TestOwnMarkerThroughGitHub(t *testing.T) {
	fake := githubtest.New(t, "acme/widgets")
	diff := "diff --git a/f.go b/f.go\n--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-a\n+b\n"
	fake.AddPR(1, "h1", "b1", diff)
	c := fake.Client(githubtest.DefaultToken)
	repo := github.Repo{Owner: "acme", Name: "widgets"}
	ctx := context.Background()

	body, err := Append("finding", &Marker{ID: "f-1", Severity: "P1", SHA: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.CreateReview(ctx, repo, 1, &github.Review{CommitID: "h1", Comments: []github.ReviewComment{
		{Body: body, Anchor: github.Anchor{Path: "f.go", Line: 1, Side: github.SideRight}},
	}}); err != nil {
		t.Fatal(err)
	}
	threads, err := c.ReviewThreads(ctx, repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := Append("lgtm", &Marker{ID: "f-1", To: StateDismissed})
	if err != nil {
		t.Fatal(err)
	}
	fake.Reply(1, threads[0].ID, "dratner", forged)
	threads, err = c.ReviewThreads(ctx, repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	cs := threads[0].Comments
	if m, own := OwnMarker(&cs[0], githubtest.DefaultLogin); !own || m.ID != "f-1" {
		t.Errorf("buggrit's own root not recognized: %+v", cs[0])
	}
	if _, own := OwnMarker(&cs[1], githubtest.DefaultLogin); own {
		t.Error("forged marker in a human reply was accepted")
	}
}

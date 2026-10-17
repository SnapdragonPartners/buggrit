package github_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
)

const compareDiff = `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,2 +1,3 @@
 package a
+var x = 1
 // end
`

func TestCompare(t *testing.T) {
	fake, c := setup(t)
	ctx := context.Background()
	fake.SetCompare("old", "new", githubtest.Comparison{Status: github.CompareAhead, Diff: compareDiff})
	fake.SetCompare("gone", "new", githubtest.Comparison{Status: github.CompareDiverged})

	if s, err := c.CompareStatus(ctx, repo, "old", "new"); err != nil || s != github.CompareAhead {
		t.Errorf("ahead: %q, %v", s, err)
	}
	if s, err := c.CompareStatus(ctx, repo, "gone", "new"); err != nil || s != github.CompareDiverged {
		t.Errorf("diverged: %q, %v", s, err)
	}
	if _, err := c.CompareStatus(ctx, repo, "unknown", "new"); !github.IsNotFound(err) {
		t.Errorf("unknown commit: err = %v, want not found", err)
	}

	d, err := c.CompareDiff(ctx, repo, "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if f := d.File("a.go"); f == nil || len(f.AddedLines()) != 1 || f.AddedLines()[0] != 2 {
		t.Errorf("diff = %+v", d)
	}

	fake.OnRequest = func(op string) int {
		if op == "GET compare" {
			return http.StatusNotAcceptable
		}
		return 0
	}
	if _, err := c.CompareDiff(ctx, repo, "old", "new"); !errors.Is(err, github.ErrDiffTooLarge) {
		t.Errorf("too large: err = %v", err)
	}
}

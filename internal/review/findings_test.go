package review

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/glob"
)

func testSubmit(t *testing.T) *submitTool {
	t.Helper()
	d := config.Defaults().Review
	scope := glob.Filter{Include: d.Include, Exclude: d.Exclude}
	return &submitTool{env: testEnv(t), inScope: scope.InScope}
}

const validFinding = `{
  "path": "internal/cache/cache.go", "line": 3, "severity": "P1", "category": "correctness",
  "confidence": "high", "title": "Nil map write when cache is cold",
  "problem": "New leaves entries nil, so Put panics.", "fix": "Initialize entries in New.",
  "class": "uninitialized map", "instances": [{"path": "internal/cache/lru.go", "line": 3}]
}`

func TestSubmitAccepts(t *testing.T) {
	s := testSubmit(t)
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"summary": "one bug", "findings": [`+validFinding+`]}`)
	if res.IsError || !res.Stop {
		t.Fatalf("result = %+v", res)
	}
	got := s.result.Findings[0]
	if got.Side != "RIGHT" || !got.Blocking() || s.result.Summary != "one bug" {
		t.Errorf("finding = %+v", got)
	}
}

func TestSubmitNormalizes(t *testing.T) {
	s := testSubmit(t)
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"findings": [{
	  "path": "./internal/cache/cache.go", "line": 3, "start_line": 3, "severity": "p2", "category": "Docs-Drift",
	  "confidence": "HIGH", "title": " t ", "problem": "p", "fix": "f"}]}`)
	if res.IsError {
		t.Fatalf("rejected: %s", res.Content)
	}
	f := s.result.Findings[0]
	if f.Path != "internal/cache/cache.go" || f.Severity != "P2" || f.Category != "docs-drift" ||
		f.Confidence != "high" || f.Title != "t" || f.StartLine != 0 {
		t.Errorf("not normalized: %+v", f)
	}
}

func TestSubmitEmpty(t *testing.T) {
	s := testSubmit(t)
	if res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"findings": []}`); !res.Stop {
		t.Errorf("empty submission not accepted: %+v", res)
	}
	s = testSubmit(t)
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{}`)
	wantErr(t, res, "findings is required")
	if res.Stop {
		t.Error("missing findings stopped the loop")
	}
}

// decodeArgs reads one JSON value; a second value or trailing text must not
// slip through as an accepted (empty) submission.
func TestSubmitRejectsTrailingData(t *testing.T) {
	for _, args := range []string{`{"findings": []} {"findings": []}`, `{"findings": []} and more`} {
		s := testSubmit(t)
		res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", args)
		wantErr(t, res, "unexpected data after the JSON object")
		if res.Stop || s.result != nil {
			t.Errorf("%s was accepted", args)
		}
	}
	s := testSubmit(t)
	if res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", " {\"findings\": []}\n "); !res.Stop {
		t.Errorf("surrounding whitespace rejected: %+v", res)
	}
}

// A missing location field doesn't hide the finding's other problems: one
// resubmission must be able to fix everything (design §5).
func TestSubmitMissingLineStillReportsFields(t *testing.T) {
	s := testSubmit(t)
	bad := strings.Replace(strings.Replace(validFinding, `"line": 3, `, ``, 1), `"P1"`, `"P7"`, 1)
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"findings": [`+bad+`]}`)
	wantErr(t, res, "line is required")
	wantErr(t, res, "severity must be")
	if strings.Contains(res.Content, "has 5 lines") || strings.Contains(res.Content, "not changed") {
		t.Errorf("anchor checks ran on a defaulted line:\n%s", res.Content)
	}
}

func TestSubmitRejects(t *testing.T) {
	with := func(old, repl string) string { return strings.Replace(validFinding, old, repl, 1) }
	tests := []struct{ name, finding, want string }{
		{"untouched file", with(`"path": "internal/cache/cache.go"`, `"path": "README.md"`), "README.md is not changed"},
		{"excluded file", with(`"path": "internal/cache/cache.go", "line": 3`, `"path": "go.sum", "line": 1`), "excluded from review"},
		{"line past EOF", with(`"line": 3,`, `"line": 99,`), "has 5 lines; line 99"},
		{"range reversed", with(`"line": 3,`, `"line": 2, "start_line": 3,`), "start_line 3 is after line 2"},
		{"bad severity", with(`"P1"`, `"P5"`), "severity must be"},
		{"bad category", with(`"correctness"`, `"bugs"`), "category must be one of"},
		{"bad confidence", with(`"high"`, `"sure"`), "confidence must be"},
		{"no title", with(`"title": "Nil map write when cache is cold"`, `"title": ""`), "title is required"},
		{"long title", with(`"title": "Nil map write when cache is cold"`, `"title": "`+strings.Repeat("x", 121)+`"`), "at most 120"},
		{"no problem", with(`"problem": "New leaves entries nil, so Put panics."`, `"problem": " "`), "problem is required"},
		{"instances without class", with(`"class": "uninitialized map", `, ``), "need a class"},
		{"bad instance", with(`{"path": "internal/cache/lru.go", "line": 3}`, `{"path": "nope.go", "line": 1}`), "instances[0]: nope.go"},
		{"LEFT on a head line", with(`"line": 3,`, `"line": 3, "side": "LEFT",`), "side LEFT needs"},
		{"RIGHT on deleted file", with(`"path": "internal/cache/cache.go", "line": 3`, `"path": "old.txt", "line": 1`), "old.txt is deleted"},
		{"bad side", with(`"line": 3,`, `"line": 3, "side": "UP",`), "side must be"},
		{"suggestion on file level", with(`"line": 3,`, `"line": 0, "suggestion": "x",`), "suggestion needs a line"},
		{"unknown field", with(`"fix":`, `"fixes": "x", "fix":`), "unknown field"},
		{"missing line", with(`"line": 3, `, ``), "line is required"},
		{"null line", with(`"line": 3,`, `"line": null,`), "line is required"},
		{"missing path", with(`"path": "internal/cache/cache.go", `, ``), "path is required"},
		{"empty instance", with(`{"path": "internal/cache/lru.go", "line": 3}`, `{}`), "instances[0]: path is required"},
		{"instance without line", with(`{"path": "internal/cache/lru.go", "line": 3}`, `{"path": "internal/cache/lru.go"}`), "instances[0]: line is required"},
		{"instance with null path", with(`{"path": "internal/cache/lru.go", "line": 3}`, `{"path": null, "line": 0}`), "instances[0]: path is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSubmit(t)
			res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"findings": [`+tt.finding+`]}`)
			wantErr(t, res, tt.want)
			if res.Stop || s.result != nil {
				t.Error("invalid submission was accepted")
			}
		})
	}
}

// A PR that adds a symlink can still get findings about it: file level, or
// on the diff's line (the link target), without the tools reading through
// the link.
func TestSubmitOnSymlink(t *testing.T) {
	diff := "diff --git a/link b/link\nnew file mode 120000\n--- /dev/null\n+++ b/link\n@@ -0,0 +1 @@\n+.git/config\n\\ No newline at end of file\n"
	d, err := github.ParseDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	ws := fstest.MapFS{
		"link":        {Data: []byte(".git/config"), Mode: fs.ModeSymlink},
		".git/config": {Data: []byte("SECRET")},
	}
	s := &submitTool{env: &toolEnv{ws: ws, diff: d}, inScope: func(string) bool { return true }}
	for _, line := range []int{0, 1} {
		res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", fmt.Sprintf(`{"findings": [{"path": "link", "line": %d,
		  "severity": "P1", "category": "security", "confidence": "high", "title": "t", "problem": "p", "fix": "f"}]}`, line))
		if res.IsError {
			t.Errorf("line %d rejected: %s", line, res.Content)
		}
	}
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"findings": [{"path": "link", "line": 0,
	  "severity": "P1", "category": "security", "confidence": "high", "title": "t", "problem": "p", "fix": "f",
	  "class": "c", "instances": [{"path": "link/x", "line": 0}]}]}`)
	wantErr(t, res, "symbolic link")
}

// A file-level finding about a deleted file is allowed (design §8).
func TestSubmitFileLevelOnDeletedFile(t *testing.T) {
	s := testSubmit(t)
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"findings": [{"path": "old.txt", "line": 0,
	  "severity": "P2", "category": "docs-drift", "confidence": "medium", "title": "t", "problem": "p", "fix": "f"}]}`)
	if res.IsError {
		t.Fatalf("rejected: %s", res.Content)
	}
}

func TestSubmitDeletedLineOnLeft(t *testing.T) {
	s := testSubmit(t)
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"findings": [{"path": "old.txt", "line": 2, "side": "LEFT",
	  "severity": "P2", "category": "docs-drift", "confidence": "medium", "title": "t", "problem": "p", "fix": "f"}]}`)
	if res.IsError {
		t.Fatalf("rejected: %s", res.Content)
	}
}

// Every problem in a submission is reported at once, so the model can fix
// them in one resubmission.
func TestSubmitReportsEveryProblem(t *testing.T) {
	s := testSubmit(t)
	bad := strings.Replace(strings.Replace(validFinding, `"P1"`, `"P9"`, 1), `"high"`, `"sure"`, 1)
	res := call(t, []toolloop.Tool{s.tool()}, "submit_findings", `{"findings": [`+validFinding+`, `+bad+`, `+
		strings.Replace(validFinding, `"line": 3,`, `"line": 77,`, 1)+`]}`)
	for _, want := range []string{"findings[1]", "severity must be", "confidence must be", "findings[2]", "line 77"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %q in:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "findings[0]") {
		t.Errorf("valid finding reported:\n%s", res.Content)
	}
}

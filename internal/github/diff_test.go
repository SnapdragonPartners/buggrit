package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// anchorQuery is one AnchorFor call recorded in the golden file.
type anchorQuery struct {
	Path   string `json:"path"`
	Result string `json:"result"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
}

func TestDiffGolden(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("testdata", "git.diff"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDiff(string(text))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	queries := []anchorQuery{
		{Path: "modified.go", End: 2},              // changed line
		{Path: "modified.go", End: 4},              // inserted line
		{Path: "modified.go", End: 5},              // context line
		{Path: "modified.go", Start: 1, End: 4},    // range inside one hunk
		{Path: "modified.go", Start: 4, End: 23},   // range across hunks: narrowed
		{Path: "modified.go", End: 15},             // between hunks: file-level
		{Path: "modified.go", End: 31},             // appended line
		{Path: "modified.go", End: 32},             // past the end
		{Path: "added.go", End: 1},                 // new file
		{Path: "new_name.go", End: 5},              // renamed file, head path
		{Path: "old_name.go", End: 5},              // base path of a rename: not in diff
		{Path: "deleted.txt", End: 1},              // deleted file: no RIGHT lines
		{Path: "blob.bin", End: 1},                 // binary
		{Path: "renamed_pure.txt", End: 1},         // rename without hunks
		{Path: "café.go", End: 1},                  // unquoted path
		{Path: "with space.go", End: 1},            // path with a space
		{Path: "nonewline.txt", Start: 1, End: 2},  // no newline at EOF
		{Path: "not/in/diff.go", End: 1},           // unknown file
		{Path: "modified.go", Start: 3, End: 3},    // equal start and end
		{Path: "modified.go", Start: 0, End: 0},    // line 0: file-level
		{Path: "script.sh", End: 1},                // mode change only
		{Path: "modified.go", Start: 25, End: 23},  // reversed range: narrowed
		{Path: "nonewline.txt", Start: 1, End: 99}, // range end outside
	}
	for i := range queries {
		q := &queries[i]
		a, err := d.AnchorFor(q.Path, q.Start, q.End)
		switch {
		case err != nil:
			q.Result = "error: " + err.Error()
		case a.FileLevel():
			q.Result = "file " + a.Path
		case a.StartLine > 0:
			q.Result = strings.Join([]string{"lines", a.Path, itoa(a.StartLine) + "-" + itoa(a.Line), string(a.Side)}, " ")
		default:
			q.Result = strings.Join([]string{"line", a.Path, itoa(a.Line), string(a.Side)}, " ")
		}
	}

	got, err := json.MarshalIndent(map[string]any{"diff": d, "anchors": queries}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "git.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("parsed diff differs from %s; rerun with -update and review the change.\ngot:\n%s", golden, got)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestParseDiffStatuses(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("testdata", "git.diff"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDiff(string(text))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]FileStatus{
		"added.go":         StatusAdded,
		"blob.bin":         StatusModified,
		"café.go":          StatusModified,
		"deleted.txt":      StatusDeleted,
		"modified.go":      StatusModified,
		"new_name.go":      StatusRenamed,
		"nonewline.txt":    StatusModified,
		"renamed_pure.txt": StatusRenamed,
		"script.sh":        StatusModified,
		"with space.go":    StatusModified,
	}
	if len(d.Files) != len(want) {
		t.Fatalf("got %d files, want %d", len(d.Files), len(want))
	}
	for path, status := range want {
		f := d.File(path)
		if f == nil {
			t.Errorf("%s: missing", path)
			continue
		}
		if f.Status != status {
			t.Errorf("%s: status %s, want %s", path, f.Status, status)
		}
	}
	if f := d.File("new_name.go"); f.OldPath != "old_name.go" {
		t.Errorf("rename old path = %q", f.OldPath)
	}
	if f := d.File("deleted.txt"); f.NewPath != "" || f.OldPath != "deleted.txt" {
		t.Errorf("deleted paths = %q -> %q", f.OldPath, f.NewPath)
	}
	if !d.File("blob.bin").Binary {
		t.Error("blob.bin not marked binary")
	}
	if got := d.File("modified.go").AddedLines(); !slices.Equal(got, []int{2, 4, 31}) {
		t.Errorf("added lines = %v, want [2 4 31]", got)
	}
	if !d.File("deleted.txt").Commentable(1, SideLeft) {
		t.Error("deleted line 1 should be commentable on LEFT")
	}
}

func TestParseDiffEdgeCases(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		d, err := ParseDiff("")
		if err != nil || len(d.Files) != 0 {
			t.Fatalf("got %v, %v", d, err)
		}
	})
	t.Run("stripped empty context line", func(t *testing.T) {
		d, err := ParseDiff("diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n\n-c\n+C\n")
		if err != nil {
			t.Fatal(err)
		}
		if !d.File("f").Commentable(3, SideRight) || !d.File("f").Commentable(2, SideRight) {
			t.Error("lines 2 and 3 should be commentable")
		}
	})
	t.Run("ambiguous rename with spaces", func(t *testing.T) {
		d, err := ParseDiff("diff --git a/x b/y b/z b/w\nsimilarity index 100%\nrename from x b/y\nrename to z b/w\n")
		if err != nil {
			t.Fatal(err)
		}
		f := d.Files[0]
		if f.OldPath != "x b/y" || f.NewPath != "z b/w" || f.Status != StatusRenamed {
			t.Errorf("got %+v", f)
		}
	})
	t.Run("deleted path with space", func(t *testing.T) {
		d, err := ParseDiff("diff --git a/a b b/a b\ndeleted file mode 100644\n--- a/a b\t\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n")
		if err != nil {
			t.Fatal(err)
		}
		if f := d.Files[0]; f.OldPath != "a b" || f.NewPath != "" || f.Path() != "a b" {
			t.Errorf("got %+v", f)
		}
	})
	t.Run("hunk without count", func(t *testing.T) {
		d, err := ParseDiff("diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -3 +3,2 @@\n-x\n+y\n+z\n")
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Files[0].AddedLines(); !slices.Equal(got, []int{3, 4}) {
			t.Errorf("added = %v", got)
		}
	})
	t.Run("CRLF content kept", func(t *testing.T) {
		d, err := ParseDiff("diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\r\n+y\r\n")
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Files[0].Hunks[0].Lines[1].Text; got != "y\r" {
			t.Errorf("text = %q", got)
		}
	})
	t.Run("binary patch data ignored", func(t *testing.T) {
		d, err := ParseDiff("diff --git a/b b/b\nindex 1..2\nGIT binary patch\nliteral 5\nMcmZ?d\n\nliteral 0\nHcmV?d00001\n\ndiff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-x\n+y\n")
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Files) != 2 || !d.Files[0].Binary || len(d.Files[1].Hunks) != 1 {
			t.Errorf("got %+v", d.Files)
		}
	})
}

// Renames between a plain path and one git must quote, in both directions,
// from real git output.
func TestParseDiffMixedQuoting(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("testdata", "mixed_quoting.diff"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDiff(string(text))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	want := []struct{ old, new string }{{"plain.txt", "café.txt"}, {"naïve.txt", "plain2.txt"}}
	if len(d.Files) != len(want) {
		t.Fatalf("got %d files", len(d.Files))
	}
	for i, w := range want {
		if f := d.Files[i]; f.OldPath != w.old || f.NewPath != w.new || f.Status != StatusRenamed {
			t.Errorf("file %d = %q -> %q (%s), want %q -> %q", i, f.OldPath, f.NewPath, f.Status, w.old, w.new)
		}
	}
	if a, err := d.AnchorFor("café.txt", 0, 21); err != nil || a.Line != 21 {
		t.Errorf("anchor = %+v, %v", a, err)
	}
	// The header alone, without rename lines, must split too.
	for _, header := range []string{
		"diff --git a/plain.txt \"b/caf\\303\\251.txt\"\n",
		"diff --git \"a/na\\303\\257ve.txt\" b/plain2.txt\n",
	} {
		if _, err := ParseDiff(header); err != nil {
			t.Errorf("header %q: %v", header, err)
		}
	}
}

func TestParseDiffRejects(t *testing.T) {
	tests := []struct{ name, diff, want string }{
		{"garbage", "hello\n", `expected "diff --git"`},
		{"short hunk", "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n", "hunk ended early"},
		{"long hunk", "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1,2 @@\n a\n b\n", "longer than its header"},
		{"stray line after hunk", "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n a\n+b\n", `expected "diff --git"`},
		{"bad header", "diff --git a/f b/f\n@@ -x +1 @@\n", "bad range"},
		{"bad line", "diff --git a/f b/f\n@@ -1 +1 @@\n?x\n", "unexpected hunk line"},
		{"unsplittable", "diff --git f g\n", "cannot split paths"},
		{"unterminated quote", "diff --git \"a/f b/f\n", "unterminated quoted path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDiff(tt.diff)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAnchorForUnknownFile(t *testing.T) {
	d := &Diff{}
	if _, err := d.AnchorFor("x", 0, 1); !errors.Is(err, ErrNotInDiff) {
		t.Errorf("error = %v, want ErrNotInDiff", err)
	}
}

func TestUnquotePath(t *testing.T) {
	tests := map[string]string{
		`"a/caf\303\251.go"`: "a/café.go",
		`"a/tab\there"`:      "a/tab\there",
		`"a/q\"uote"`:        `a/q"uote`,
		`"a/back\\slash"`:    `a/back\slash`,
		`plain`:              "plain",
		`"unterminated`:      `"unterminated`,
	}
	for in, want := range tests {
		if got := unquotePath(in); got != want {
			t.Errorf("unquotePath(%s) = %q, want %q", in, got, want)
		}
	}
}

// A mode change is recorded, with or without text hunks; incremental scope
// matches it against the PR's diff (design §7 rule 2).
func TestParseDiffModeChange(t *testing.T) {
	d, err := ParseDiff(`diff --git a/run.sh b/run.sh
old mode 100644
new mode 100755
diff --git a/lib.sh b/lib.sh
old mode 100644
new mode 100755
--- a/lib.sh
+++ b/lib.sh
@@ -1 +1 @@
-a
+b
diff --git a/plain.sh b/plain.sh
--- a/plain.sh
+++ b/plain.sh
@@ -1 +1 @@
-a
+b
`)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{"run.sh": true, "lib.sh": true, "plain.sh": false} {
		if f := d.File(path); f == nil || f.ModeChanged != want {
			t.Errorf("%s: %+v, want ModeChanged %v", path, f, want)
		}
	}
}

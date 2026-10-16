package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

func TestCleanPath(t *testing.T) {
	ok := map[string]string{
		"":                ".",
		"/":               ".",
		"./a.go":          "a.go",
		"a/./b.go":        "a/b.go",
		"a/b/../c.go":     "a/c.go",
		" internal/x.go ": "internal/x.go",
		".github/ci.yml":  ".github/ci.yml",
		".gitignore":      ".gitignore",
	}
	for in, want := range ok {
		got, err := cleanPath(in)
		if err != nil || got != want {
			t.Errorf("cleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/etc/passwd", "../x", "a/../../x", ".git", ".git/config", "sub/.git/HEAD"} {
		if got, err := cleanPath(bad); err == nil {
			t.Errorf("cleanPath(%q) = %q, want an error", bad, got)
		}
	}
}

func TestReadFile(t *testing.T) {
	tools := readOnlyTools(testEnv(t))
	wantOK(t, call(t, tools, "read_file", `{"path": "internal/cache/cache.go"}`),
		"internal/cache/cache.go (lines 1-5 of 5)", "     3| func (c *Cache) Put")
	res := call(t, tools, "read_file", `{"path": "internal/cache/cache.go", "start_line": 2, "end_line": 3}`)
	wantOK(t, res, "(lines 2-3 of 5)")
	if strings.Contains(res.Content, "func New") {
		t.Error("window included line 4")
	}
	wantErr(t, call(t, tools, "read_file", `{"path": "internal/cache/cache.go", "start_line": 9}`), "past the end")
	wantErr(t, call(t, tools, "read_file", `{"path": "internal/cache/cache.go", "start_line": 3, "end_line": 2}`), "after end_line")
	wantErr(t, call(t, tools, "read_file", `{"path": "nope.go"}`), "no such file")
	wantErr(t, call(t, tools, "read_file", `{"path": "blob.bin"}`), "binary")
	wantErr(t, call(t, tools, "read_file", `{"path": ".git/config"}`), "not available")
	wantErr(t, call(t, tools, "read_file", `{"path": "../etc/passwd"}`), "inside the repository")
	wantErr(t, call(t, tools, "read_file", `{"path": "x", "lines": 3}`), "unknown field")
	wantErr(t, call(t, tools, "read_file", `{"path": "internal"}`), "read internal")
}

func TestReadFileTruncates(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= maxReadLines+500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	env := &toolEnv{ws: fstest.MapFS{"big.txt": {Data: []byte(b.String())}}, diff: testDiffParsed(t)}
	res := call(t, readOnlyTools(env), "read_file", `{"path": "big.txt"}`)
	wantOK(t, res, fmt.Sprintf("call read_file with start_line=%d", maxReadLines+1))
	if strings.Contains(res.Content, fmt.Sprintf("line %d\n", maxReadLines+1)) {
		t.Error("read past the line cap")
	}
}

func TestGrep(t *testing.T) {
	tools := readOnlyTools(testEnv(t))
	wantOK(t, call(t, tools, "grep", `{"pattern": "map\\["}`),
		"internal/cache/cache.go:2:", "internal/cache/lru.go:3:")
	res := call(t, tools, "grep", `{"pattern": "map", "path_glob": "internal/**/lru.go"}`)
	wantOK(t, res, "lru.go")
	if strings.Contains(res.Content, "cache.go") {
		t.Error("path_glob not applied")
	}
	wantOK(t, call(t, tools, "grep", `{"pattern": "CALL NEW", "ignore_case": true}`), "README.md:2:")
	if res := call(t, tools, "grep", `{"pattern": "SECRET"}`); res.Content != "no matches" {
		t.Errorf(".git searched: %s", res.Content)
	}
	wantErr(t, call(t, tools, "grep", `{"pattern": "("}`), "bad pattern")
	wantErr(t, call(t, tools, "grep", `{"pattern": "x", "path_glob": "a**"}`), "bad path_glob")
}

func TestGrepTruncates(t *testing.T) {
	var b strings.Builder
	for range maxGrepMatches + 50 {
		b.WriteString("hit\n")
	}
	env := &toolEnv{ws: fstest.MapFS{"many.txt": {Data: []byte(b.String())}}, diff: testDiffParsed(t)}
	wantOK(t, call(t, readOnlyTools(env), "grep", `{"pattern": "hit"}`), fmt.Sprintf("stopped after %d matches", maxGrepMatches))
}

func TestListDir(t *testing.T) {
	tools := readOnlyTools(testEnv(t))
	res := call(t, tools, "list_dir", `{}`)
	wantOK(t, res, "internal/\n", "README.md\n", "sub/\n")
	if strings.Contains(res.Content, ".git") {
		t.Errorf(".git listed:\n%s", res.Content)
	}
	wantOK(t, call(t, tools, "list_dir", `{"path": "internal/cache"}`), "cache.go\n", "lru.go\n")
	if res := call(t, tools, "list_dir", `{"path": "sub"}`); strings.Contains(res.Content, ".git") {
		t.Errorf("nested .git listed: %s", res.Content)
	}
	wantErr(t, call(t, tools, "list_dir", `{"path": "nope"}`), "no such file")
}

func TestGetDiff(t *testing.T) {
	tools := readOnlyTools(testEnv(t))
	wantOK(t, call(t, tools, "get_diff", `{}`),
		"internal/cache/cache.go (modified, +2 -1)", "go.sum (modified, +1 -1)", "old.txt (deleted, +0 -2)")
	wantOK(t, call(t, tools, "get_diff", `{"path": "internal/cache/cache.go"}`),
		"=== internal/cache/cache.go", "@@ -1,4 +1,5 @@", "     3 + func (c *Cache) Put", "       - type Cache struct{}")
	wantOK(t, call(t, tools, "get_diff", `{"path": "old.txt"}`), "old.txt (deleted)")
	wantErr(t, call(t, tools, "get_diff", `{"path": "README.md"}`), "not changed")
}

// OpenDir confines reads to the directory: a symlink pointing outside is
// refused (design §10).
func TestOpenDirConfines(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("fine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("ok.txt", filepath.Join(dir, "inside.txt")); err != nil {
		t.Fatal(err)
	}
	ws, closeFn, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeFn() }()
	tools := readOnlyTools(&toolEnv{ws: ws, diff: testDiffParsed(t)})
	wantOK(t, call(t, tools, "read_file", `{"path": "ok.txt"}`), "fine")
	// An in-repo link is refused too, naming its target so the model can
	// read that path directly.
	wantErr(t, call(t, tools, "read_file", `{"path": "inside.txt"}`), "symbolic link to ok.txt")
	res := call(t, tools, "read_file", `{"path": "escape.txt"}`)
	if !res.IsError || strings.Contains(res.Content, "SECRET") {
		t.Errorf("symlink escape not refused: %+v", res)
	}
	if strings.Contains(res.Content, outside) {
		t.Errorf("error leaks the host path: %s", res.Content)
	}
	if res := call(t, tools, "grep", `{"pattern": "SECRET"}`); res.Content != "no matches" {
		t.Errorf("grep followed an escaping symlink: %s", res.Content)
	}
}

// A symlink pointing into .git must not expose it (design §10).
func TestSymlinkIntoGitRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("token=SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git", filepath.Join(dir, "metadata")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ws, closeFn, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeFn() }()
	tools := readOnlyTools(&toolEnv{ws: ws, diff: testDiffParsed(t)})
	for _, tc := range []struct{ tool, args string }{
		{"read_file", `{"path": "metadata/config"}`},
		{"list_dir", `{"path": "metadata"}`},
	} {
		res := call(t, tools, tc.tool, tc.args)
		if !res.IsError || strings.Contains(res.Content, "SECRET") || strings.Contains(res.Content, "config\n") {
			t.Errorf("%s through a .git symlink: %+v", tc.tool, res)
		}
	}
	// Following the named target hits the .git rule.
	wantErr(t, call(t, tools, "read_file", `{"path": ".git/config"}`), "not available")
	if res := call(t, tools, "grep", `{"pattern": "SECRET"}`); res.Content != "no matches" {
		t.Errorf("grep reached .git: %s", res.Content)
	}
}

// A submodule's .git is a regular file; grep must skip it too.
func TestGrepSkipsGitFile(t *testing.T) {
	env := &toolEnv{ws: fstest.MapFS{"sub/.git": {Data: []byte("gitdir: SECRET\n")}}, diff: testDiffParsed(t)}
	if res := call(t, readOnlyTools(env), "grep", `{"pattern": "SECRET"}`); res.Content != "no matches" {
		t.Errorf("grep searched a .git file: %s", res.Content)
	}
}

// One enormous line can't push a result past the byte cap.
func TestLongLinesClipped(t *testing.T) {
	long := strings.Repeat("x", 400<<10)
	diff := "diff --git a/min.js b/min.js\n--- a/min.js\n+++ b/min.js\n@@ -1 +1 @@\n-" + long + "\n+" + long + "\n"
	d, err := github.ParseDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	env := &toolEnv{ws: fstest.MapFS{"min.js": {Data: []byte(long + "\n")}}, diff: d}
	tools := readOnlyTools(env)
	for _, tc := range []struct{ tool, args string }{
		{"read_file", `{"path": "min.js"}`},
		{"get_diff", `{"path": "min.js"}`},
	} {
		res := call(t, tools, tc.tool, tc.args)
		wantOK(t, res, "clipped")
		if len(res.Content) > maxResultBytes+2*maxReadLineBytes {
			t.Errorf("%s returned %d bytes", tc.tool, len(res.Content))
		}
	}
}

// A long diff pages with offset, so deleted lines past the cap stay
// reachable even though the head checkout doesn't have them.
func TestGetDiffPages(t *testing.T) {
	var b strings.Builder
	n := 30000
	fmt.Fprintf(&b, "diff --git a/gone.txt b/gone.txt\ndeleted file mode 100644\n--- a/gone.txt\n+++ /dev/null\n@@ -1,%d +0,0 @@\n", n)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "-deleted line %d\n", i)
	}
	d, err := github.ParseDiff(b.String())
	if err != nil {
		t.Fatal(err)
	}
	tools := readOnlyTools(&toolEnv{ws: fstest.MapFS{}, diff: d})
	seen := map[string]bool{}
	offset := 0
	for page := 0; page < 20; page++ {
		res := call(t, tools, "get_diff", fmt.Sprintf(`{"path": "gone.txt", "offset": %d}`, offset))
		wantOK(t, res)
		for _, line := range strings.Split(res.Content, "\n") {
			if i := strings.Index(line, "deleted line "); i >= 0 {
				seen[line[i:]] = true
			}
		}
		_, rest, found := strings.Cut(res.Content, `"gone.txt" and offset `)
		if !found {
			break
		}
		next, _, _ := strings.Cut(rest, " ")
		if _, err := fmt.Sscan(next, &offset); err != nil {
			t.Fatalf("bad continuation %q", next)
		}
	}
	if len(seen) != n {
		t.Errorf("paging reached %d of %d deleted lines", len(seen), n)
	}
	wantErr(t, call(t, tools, "get_diff", `{"path": "gone.txt", "offset": -1}`), "must not be negative")
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo", 2); got != "h…" {
		t.Errorf("truncate split a rune: %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
}

package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// The test repository: cache.go changes lines 2-3; README.md is untouched;
// go.sum changes but is excluded by the default config.
const testDiff = `diff --git a/internal/cache/cache.go b/internal/cache/cache.go
--- a/internal/cache/cache.go
+++ b/internal/cache/cache.go
@@ -1,4 +1,5 @@
 package cache
-type Cache struct{}
+type Cache struct{ entries map[string]int }
+func (c *Cache) Put(k string, v int) { c.entries[k] = v }
 func New() *Cache { return &Cache{} }
 // end
diff --git a/go.sum b/go.sum
--- a/go.sum
+++ b/go.sum
@@ -1 +1 @@
-a v1
+a v2
diff --git a/old.txt b/old.txt
deleted file mode 100644
--- a/old.txt
+++ /dev/null
@@ -1,2 +0,0 @@
-first
-second
`

func testWorkspace() fstest.MapFS {
	return fstest.MapFS{
		"internal/cache/cache.go": {Data: []byte("package cache\ntype Cache struct{ entries map[string]int }\nfunc (c *Cache) Put(k string, v int) { c.entries[k] = v }\nfunc New() *Cache { return &Cache{} }\n// end\n")},
		"internal/cache/lru.go":   {Data: []byte("package cache\n// TODO: same map bug\nvar lru = map[string]int{}\n")},
		"README.md":               {Data: []byte("# Cache\nCall New, then Put.\n")},
		"CLAUDE.md":               {Data: []byte("Always initialize maps in constructors.\n")},
		"go.sum":                  {Data: []byte("a v2\n")},
		"blob.bin":                {Data: []byte{0, 1, 2}},
		".git/config":             {Data: []byte("[credential] token=SECRET\n")},
		"sub/.git/HEAD":           {Data: []byte("SECRET\n")},
	}
}

func testDiffParsed(t *testing.T) *github.Diff {
	t.Helper()
	d, err := github.ParseDiff(testDiff)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func testEnv(t *testing.T) *toolEnv {
	t.Helper()
	return &toolEnv{ws: testWorkspace(), diff: testDiffParsed(t)}
}

// call runs one tool from tools by name with JSON args.
func call(t *testing.T, tools []toolloop.Tool, name, args string) toolloop.ToolResult {
	t.Helper()
	for _, tool := range tools {
		if tool.Definition.Name == name {
			res, err := tool.Execute(context.Background(), llms.ToolCall{ID: "c1", Name: name, Parameters: json.RawMessage(args)})
			if err != nil {
				t.Fatalf("%s(%s): Execute error %v", name, args, err)
			}
			return res
		}
	}
	t.Fatalf("no tool %s", name)
	return toolloop.ToolResult{}
}

func wantOK(t *testing.T, res toolloop.ToolResult, contains ...string) {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", res.Content)
	}
	for _, c := range contains {
		if !strings.Contains(res.Content, c) {
			t.Errorf("result missing %q:\n%s", c, res.Content)
		}
	}
}

func wantErr(t *testing.T, res toolloop.ToolResult, contains string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected a tool error containing %q, got:\n%s", contains, res.Content)
	}
	if !strings.Contains(res.Content, contains) {
		t.Errorf("error %q does not contain %q", res.Content, contains)
	}
}

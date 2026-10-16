package review

import (
	"testing"
	"testing/fstest"

	"github.com/SnapdragonPartners/buggrit/internal/github"
)

func lineFinding(path string, start, line int) *Finding {
	return &Finding{Path: path, Side: "RIGHT", Category: "correctness", Title: "Nil map", StartLine: start, Line: line}
}

// The fingerprint follows the anchored text, not the line number, so a
// finding raised again after code above it moved still matches.
func TestFingerprintFollowsText(t *testing.T) {
	d := testDiffParsed(t)
	ws := testWorkspace()
	before := fingerprint(lineFinding("internal/cache/cache.go", 0, 3), ws, d)

	moved := fstest.MapFS{"internal/cache/cache.go": {Data: []byte("package cache\n\n// added\ntype Cache struct{ entries map[string]int }\nfunc (c *Cache) Put(k string, v int) {   c.entries[k] = v }\n")}}
	if after := fingerprint(lineFinding("internal/cache/cache.go", 0, 5), moved, d); after != before {
		t.Error("same text at a moved line (and different spacing) changed the fingerprint")
	}
	if other := fingerprint(lineFinding("internal/cache/cache.go", 0, 2), ws, d); other == before {
		t.Error("different text has the same fingerprint")
	}
	cat := lineFinding("internal/cache/cache.go", 0, 3)
	cat.Category = "concurrency"
	if fingerprint(cat, ws, d) == before {
		t.Error("category isn't part of the fingerprint")
	}
	if fingerprint(lineFinding("internal/cache/cache.go", 2, 3), ws, d) == before {
		t.Error("a range and its last line have the same fingerprint")
	}
}

func TestFingerprintLeftAndFileLevel(t *testing.T) {
	d := testDiffParsed(t)
	ws := testWorkspace()
	left := &Finding{Path: "old.txt", Side: "LEFT", Category: "correctness", Line: 2}
	first := &Finding{Path: "old.txt", Side: "LEFT", Category: "correctness", Line: 1}
	if fingerprint(left, ws, d) == fingerprint(first, ws, d) {
		t.Error("different deleted lines have the same fingerprint")
	}
	a := &Finding{Path: "internal/cache/cache.go", Category: "docs-drift", Title: "README  is stale"}
	b := &Finding{Path: "internal/cache/cache.go", Category: "docs-drift", Title: "readme is stale"}
	c := &Finding{Path: "internal/cache/cache.go", Category: "docs-drift", Title: "Changelog is stale"}
	if fingerprint(a, ws, d) != fingerprint(b, ws, d) || fingerprint(a, ws, d) == fingerprint(c, ws, d) {
		t.Error("file-level fingerprints should follow the normalized title")
	}
}

func TestAnchorAndSuggestion(t *testing.T) {
	d := testDiffParsed(t)
	tests := []struct {
		name     string
		f        Finding
		want     github.Anchor
		keepSugg bool
	}{
		{"line", Finding{Path: "internal/cache/cache.go", Side: "RIGHT", Line: 3, Suggestion: "x"},
			github.Anchor{Path: "internal/cache/cache.go", Side: github.SideRight, Line: 3}, true},
		{"range", Finding{Path: "internal/cache/cache.go", Side: "RIGHT", StartLine: 2, Line: 3, Suggestion: "x"},
			github.Anchor{Path: "internal/cache/cache.go", Side: github.SideRight, StartLine: 2, Line: 3}, true},
		{"deleted line", Finding{Path: "old.txt", Side: "LEFT", StartLine: 1, Line: 2},
			github.Anchor{Path: "old.txt", Side: github.SideLeft, StartLine: 1, Line: 2}, false},
		{"file level", Finding{Path: "internal/cache/cache.go", Side: "RIGHT", Line: 0, Suggestion: "x"},
			github.Anchor{Path: "internal/cache/cache.go"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Candidate{Finding: tt.f, Anchor: anchor(&tt.f, d)}
			if c.Anchor != tt.want {
				t.Errorf("anchor = %+v, want %+v", c.Anchor, tt.want)
			}
			if c.HasSuggestion() != tt.keepSugg {
				t.Errorf("HasSuggestion = %v", c.HasSuggestion())
			}
		})
	}
	// A range AnchorFor had to narrow can't carry the suggestion: it would
	// replace only the last line.
	narrowed := Candidate{Finding: Finding{Path: "internal/cache/cache.go", StartLine: 2, Line: 3, Suggestion: "x"},
		Anchor: github.Anchor{Path: "internal/cache/cache.go", Side: github.SideRight, Line: 3}}
	if narrowed.HasSuggestion() {
		t.Error("narrowed range kept its suggestion")
	}
}

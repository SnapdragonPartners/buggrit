package glob_test

import (
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/glob"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"**", "a.go", true},
		{"**", "a/b/c.go", true},
		{"*.md", "README.md", true},
		{"*.md", "docs/design.md", false},
		{"docs/*.md", "docs/design.md", true},
		{"docs/*.md", "docs/adr/0001.md", false},
		{"docs/**", "docs/adr/0001.md", true},
		{"docs/**", "docs", true},
		{"docs/**", "documents/x", false},
		{"**/*.pb.go", "api.pb.go", true},
		{"**/*.pb.go", "internal/api/v1/api.pb.go", true},
		{"**/*.pb.go", "internal/api.go", false},
		{"vendor/**", "vendor/github.com/x/y.go", true},
		{"vendor/**", "internal/vendor/y.go", false},
		{"**/vendor/**", "internal/vendor/y.go", true},
		{"go.sum", "go.sum", true},
		{"go.sum", "sub/go.sum", false},
		{"**/*.lock", "web/yarn.lock", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"a/**/**/b", "a/x/b", true},
		{"internal/?ithub/*.go", "internal/github/diff.go", true},
		{"[ab].go", "c.go", false},
		{"[", "[", false}, // malformed: matches nothing
	}
	for _, tt := range tests {
		if got := glob.Match(tt.pattern, tt.name); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, ok := range []string{"**", "vendor/**", "**/*.pb.go", "a/[bc]/d", `a\*b`} {
		if err := glob.Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "a**", "**b/c", "[", "a/[z-a"} {
		if err := glob.Validate(bad); err == nil {
			t.Errorf("Validate(%q) succeeded", bad)
		}
	}
}

func TestDefaultFilter(t *testing.T) {
	d := config.Defaults().Review
	f := glob.Filter{Include: d.Include, Exclude: d.Exclude}
	for name, want := range map[string]bool{
		"internal/github/diff.go":       true,
		"docs/design.md":                true,
		"go.sum":                        false,
		"go.mod":                        true,
		"vendor/x/y.go":                 false,
		"internal/api/api.pb.go":        false,
		"internal/gen/types_gen.go":     false,
		"web/package-lock.json":         true,
		"web/yarn.lock":                 false,
		"internal/review/finder.go":     true,
		"internal/review/finder_gen.go": false,
	} {
		if got := f.InScope(name); got != want {
			t.Errorf("InScope(%q) = %v, want %v", name, got, want)
		}
	}
}

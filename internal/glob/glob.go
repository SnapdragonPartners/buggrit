// Package glob matches slash-separated repository paths against the patterns
// in review.include and review.exclude (docs/design.md §4).
//
// A pattern is split on "/". The segment "**" matches zero or more whole path
// segments; every other segment is matched against one path segment with
// path.Match ("*", "?", "[…]", "\" escapes). So "vendor/**" matches everything
// under vendor/, "**/*.pb.go" matches a .pb.go file at any depth, and "*.md"
// matches only top-level Markdown files.
package glob

import (
	"fmt"
	"path"
	"strings"
)

// Validate reports a malformed pattern.
func Validate(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("empty pattern")
	}
	if strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("pattern %q must be relative to the repository root", pattern)
	}
	for seg := range strings.SplitSeq(pattern, "/") {
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return fmt.Errorf("pattern %q: ** must be a whole path segment", pattern)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("pattern %q: %w", pattern, err)
		}
	}
	return nil
}

// Match reports whether name matches pattern. A malformed pattern matches
// nothing; config validation rejects those up front.
func Match(pattern, name string) bool {
	return match(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func match(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse runs of ** and try every split point.
			for len(pat) > 0 && pat[0] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return true
			}
			for i := range name {
				if match(pat, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// Filter decides which paths a review covers: a path is in scope if it
// matches some include pattern and no exclude pattern.
type Filter struct {
	Include []string
	Exclude []string
}

// InScope reports whether name is reviewed.
func (f Filter) InScope(name string) bool {
	included := false
	for _, p := range f.Include {
		if Match(p, name) {
			included = true
			break
		}
	}
	if !included {
		return false
	}
	for _, p := range f.Exclude {
		if Match(p, name) {
			return false
		}
	}
	return true
}

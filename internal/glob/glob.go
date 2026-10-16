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
	"unicode/utf8"
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
		if err := checkRanges(seg); err != nil {
			return fmt.Errorf("pattern %q: %w", pattern, err)
		}
	}
	return nil
}

// checkRanges rejects a character class range whose bounds are reversed,
// such as [z-a]. path.Match accepts those and matches nothing, so a typo
// would silently drop files from review. It assumes path.Match has already
// accepted seg, so brackets are balanced.
func checkRanges(seg string) error {
	// next returns the character at i, honoring a backslash escape, and the
	// index after it.
	next := func(i int) (rune, int) {
		if seg[i] == '\\' && i+1 < len(seg) {
			i++
		}
		r, n := utf8.DecodeRuneInString(seg[i:])
		return r, i + n
	}
	for i := 0; i < len(seg); {
		switch seg[i] {
		case '\\':
			_, i = next(i)
			continue
		case '[':
		default:
			i++
			continue
		}
		i++
		if i < len(seg) && seg[i] == '^' {
			i++
		}
		for i < len(seg) && seg[i] != ']' {
			lo, j := next(i)
			i = j
			if i+1 < len(seg) && seg[i] == '-' && seg[i+1] != ']' {
				hi, k := next(i + 1)
				if lo > hi {
					return fmt.Errorf("character range %c-%c is reversed", lo, hi)
				}
				i = k
			}
		}
		i++ // the closing ]
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

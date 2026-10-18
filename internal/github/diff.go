package github

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Side is the side of a diff a review comment attaches to, in GitHub's terms.
type Side string

// Diff sides. RIGHT is the head version (added and context lines); LEFT is
// the base version (deleted lines).
const (
	SideRight Side = "RIGHT"
	SideLeft  Side = "LEFT"
)

// FileStatus says how a diff changes a file.
type FileStatus string

// File statuses.
const (
	StatusModified FileStatus = "modified"
	StatusAdded    FileStatus = "added"
	StatusDeleted  FileStatus = "deleted"
	StatusRenamed  FileStatus = "renamed"
)

// LineKind is the kind of one line in a hunk.
type LineKind byte

// Line kinds, using the unified-diff prefix characters.
const (
	LineContext LineKind = ' '
	LineAdded   LineKind = '+'
	LineDeleted LineKind = '-'
)

// MarshalText renders a LineKind as its prefix character, so golden files stay
// readable.
func (k LineKind) MarshalText() ([]byte, error) { return []byte{byte(k)}, nil }

// DiffLine is one line of a hunk. Old is its line number in the base file and
// New its line number in the head file; the side it doesn't exist on is 0.
type DiffLine struct {
	Text string   `json:"text"`
	Old  int      `json:"old,omitempty"`
	New  int      `json:"new,omitempty"`
	Kind LineKind `json:"kind"`
}

// Hunk is one @@ section of a file diff.
type Hunk struct {
	Lines    []DiffLine `json:"lines"`
	OldStart int        `json:"old_start"`
	OldLines int        `json:"old_lines"`
	NewStart int        `json:"new_start"`
	NewLines int        `json:"new_lines"`
}

// FileDiff is the diff of one file. OldPath is empty for an added file and
// NewPath is empty for a deleted one.
type FileDiff struct {
	OldPath string     `json:"old_path,omitempty"`
	NewPath string     `json:"new_path,omitempty"`
	Status  FileStatus `json:"status"`
	Hunks   []Hunk     `json:"hunks,omitempty"`
	Binary  bool       `json:"binary,omitempty"`
	// ModeChanged is set when the file's mode changes (git writes "old mode"
	// and "new mode" only then).
	ModeChanged bool `json:"mode_changed,omitempty"`
}

// Path is the path a review comment on this file uses: the head path, or the
// base path for a deleted file.
func (f *FileDiff) Path() string {
	if f.NewPath != "" {
		return f.NewPath
	}
	return f.OldPath
}

// Commentable reports whether GitHub accepts a line comment at line on side:
// the line must appear in one of the file's hunks on that side.
func (f *FileDiff) Commentable(line int, side Side) bool {
	return f.hunkIndex(line, side) >= 0
}

// CommentableRange reports whether a multi-line comment from start to end
// (inclusive, on one side) is accepted: both ends must be in the same hunk.
func (f *FileDiff) CommentableRange(start, end int, side Side) bool {
	if start > end {
		return false
	}
	i := f.hunkIndex(start, side)
	return i >= 0 && i == f.hunkIndex(end, side)
}

// hunkIndex returns the index of the hunk containing line on side, or -1.
func (f *FileDiff) hunkIndex(line int, side Side) int {
	if line <= 0 {
		return -1
	}
	for i := range f.Hunks {
		for _, l := range f.Hunks[i].Lines {
			switch {
			case side == SideRight && l.Kind != LineDeleted && l.New == line,
				side == SideLeft && l.Kind == LineDeleted && l.Old == line:
				return i
			}
		}
	}
	return -1
}

// AddedLines returns the head line numbers this diff adds, in order.
func (f *FileDiff) AddedLines() []int {
	var out []int
	for i := range f.Hunks {
		for _, l := range f.Hunks[i].Lines {
			if l.Kind == LineAdded {
				out = append(out, l.New)
			}
		}
	}
	return out
}

// Diff is a parsed multi-file unified diff.
type Diff struct {
	Files []FileDiff `json:"files"`
}

// File returns the diff of the file whose comment path (see FileDiff.Path) is
// path, or nil if the diff doesn't touch it.
func (d *Diff) File(path string) *FileDiff {
	for i := range d.Files {
		if d.Files[i].Path() == path {
			return &d.Files[i]
		}
	}
	return nil
}

// Anchor is where a finding's thread attaches. A zero Line means a file-level
// comment (subject_type: file).
type Anchor struct {
	Path      string
	Side      Side
	Line      int
	StartLine int
}

// FileLevel reports whether the anchor is a file-level comment.
func (a Anchor) FileLevel() bool { return a.Line == 0 }

// ErrNotInDiff means a path isn't part of the PR diff, so no comment of any
// kind can be anchored to it.
var ErrNotInDiff = errors.New("file is not in the pull request diff")

// AnchorFor maps a finding on head lines start..end of path to the anchor
// GitHub will accept. A range that isn't commentable is narrowed to end alone,
// and a line that isn't commentable falls back to a file-level comment
// (design §8). start of 0 means a single line.
func (d *Diff) AnchorFor(path string, start, end int) (Anchor, error) {
	f := d.File(path)
	if f == nil {
		return Anchor{}, fmt.Errorf("%s: %w", path, ErrNotInDiff)
	}
	if start == end {
		start = 0
	}
	switch {
	case start > 0 && f.CommentableRange(start, end, SideRight):
		return Anchor{Path: path, Side: SideRight, StartLine: start, Line: end}, nil
	case f.Commentable(end, SideRight):
		return Anchor{Path: path, Side: SideRight, Line: end}, nil
	}
	return Anchor{Path: path}, nil
}

// ParseDiff parses a git unified diff, as GitHub returns for a pull request
// with the diff media type.
func ParseDiff(text string) (*Diff, error) {
	p := diffParser{lines: strings.SplitAfter(text, "\n")}
	if err := p.parse(); err != nil {
		return nil, err
	}
	return &Diff{Files: p.files}, nil
}

type diffParser struct {
	files []FileDiff
	lines []string
	pos   int
}

func (p *diffParser) parse() error {
	for p.pos < len(p.lines) {
		line := trimEOL(p.lines[p.pos])
		if !strings.HasPrefix(line, "diff --git ") {
			if strings.TrimSpace(line) != "" {
				return p.errorf("expected \"diff --git\", got %q", line)
			}
			p.pos++
			continue
		}
		f, err := p.parseFile()
		if err != nil {
			return err
		}
		p.files = append(p.files, f)
	}
	return nil
}

func (p *diffParser) errorf(format string, args ...any) error {
	return fmt.Errorf("diff line %d: %s", p.pos+1, fmt.Sprintf(format, args...))
}

// parseFile parses one file section starting at its "diff --git" line.
func (p *diffParser) parseFile() (FileDiff, error) {
	oldPath, newPath, err := splitGitHeader(strings.TrimPrefix(trimEOL(p.lines[p.pos]), "diff --git "))
	if err != nil {
		return FileDiff{}, p.errorf("%v", err)
	}
	f := FileDiff{OldPath: oldPath, NewPath: newPath, Status: StatusModified}
	p.pos++

	// Extended header lines, up to the first hunk or the next file.
	for p.pos < len(p.lines) {
		line := trimEOL(p.lines[p.pos])
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "diff --git ") {
			break
		}
		headerLine(&f, line)
		p.pos++
	}
	switch f.Status {
	case StatusAdded:
		f.OldPath = ""
	case StatusDeleted:
		f.NewPath = ""
	case StatusModified:
		if f.OldPath != f.NewPath {
			f.Status = StatusRenamed
		}
	case StatusRenamed:
	}

	for p.pos < len(p.lines) && strings.HasPrefix(p.lines[p.pos], "@@") {
		h, err := p.parseHunk()
		if err != nil {
			return FileDiff{}, err
		}
		f.Hunks = append(f.Hunks, h)
	}
	return f, nil
}

// headerLine applies one extended header line. Lines it doesn't need (index,
// similarity, binary patch data) are ignored.
func headerLine(f *FileDiff, line string) {
	switch {
	case strings.HasPrefix(line, "new mode "):
		f.ModeChanged = true
	case strings.HasPrefix(line, "new file mode"):
		f.Status = StatusAdded
	case strings.HasPrefix(line, "deleted file mode"):
		f.Status = StatusDeleted
	case strings.HasPrefix(line, "rename from "):
		f.Status = StatusRenamed
		f.OldPath = unquotePath(strings.TrimPrefix(line, "rename from "))
	case strings.HasPrefix(line, "rename to "):
		f.Status = StatusRenamed
		f.NewPath = unquotePath(strings.TrimPrefix(line, "rename to "))
	case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
		f.Binary = true
	case f.Binary:
		// "GIT binary patch" data runs to the next file; nothing in it
		// is a header.
	case strings.HasPrefix(line, "--- "):
		if path, ok := stripPrefix(strings.TrimPrefix(line, "--- "), "a/"); ok {
			f.OldPath = path
		}
	case strings.HasPrefix(line, "+++ "):
		if path, ok := stripPrefix(strings.TrimPrefix(line, "+++ "), "b/"); ok {
			f.NewPath = path
		}
	}
}

// parseHunk parses one hunk starting at its @@ line.
func (p *diffParser) parseHunk() (Hunk, error) {
	h, err := parseHunkHeader(trimEOL(p.lines[p.pos]))
	if err != nil {
		return Hunk{}, p.errorf("%v", err)
	}
	p.pos++
	oldNo, newNo := h.OldStart, h.NewStart
	oldLeft, newLeft := h.OldLines, h.NewLines
	for (oldLeft > 0 || newLeft > 0) && p.pos < len(p.lines) {
		raw := trimEOL(p.lines[p.pos])
		if raw == "" && p.lines[p.pos] == "" {
			break
		}
		if strings.HasPrefix(raw, `\`) { // "\ No newline at end of file"
			p.pos++
			continue
		}
		kind, text := LineKind(' '), ""
		if raw != "" { // some tools strip the space from empty context lines
			kind, text = LineKind(raw[0]), raw[1:]
		}
		dl := DiffLine{Kind: kind, Text: text}
		switch kind {
		case LineContext:
			dl.Old, dl.New = oldNo, newNo
			oldNo, newNo = oldNo+1, newNo+1
			oldLeft, newLeft = oldLeft-1, newLeft-1
		case LineAdded:
			dl.New = newNo
			newNo++
			newLeft--
		case LineDeleted:
			dl.Old = oldNo
			oldNo++
			oldLeft--
		default:
			return Hunk{}, p.errorf("unexpected hunk line %q", raw)
		}
		if oldLeft < 0 || newLeft < 0 {
			return Hunk{}, p.errorf("hunk is longer than its header says")
		}
		h.Lines = append(h.Lines, dl)
		p.pos++
	}
	if oldLeft > 0 || newLeft > 0 {
		return Hunk{}, p.errorf("hunk ended early: %d old and %d new lines missing", oldLeft, newLeft)
	}
	// A trailing "\ No newline at end of file" belongs to this hunk.
	for p.pos < len(p.lines) && strings.HasPrefix(p.lines[p.pos], `\`) {
		p.pos++
	}
	return h, nil
}

// parseHunkHeader parses "@@ -a,b +c,d @@ optional section".
func parseHunkHeader(line string) (Hunk, error) {
	rest, ok := strings.CutPrefix(line, "@@ -")
	if !ok {
		return Hunk{}, fmt.Errorf("malformed hunk header %q", line)
	}
	ranges, _, ok := strings.Cut(rest, " @@")
	if !ok {
		return Hunk{}, fmt.Errorf("malformed hunk header %q", line)
	}
	oldR, newR, ok := strings.Cut(ranges, " +")
	if !ok {
		return Hunk{}, fmt.Errorf("malformed hunk header %q", line)
	}
	var h Hunk
	var err error
	if h.OldStart, h.OldLines, err = parseRange(oldR); err != nil {
		return Hunk{}, fmt.Errorf("hunk header %q: %w", line, err)
	}
	if h.NewStart, h.NewLines, err = parseRange(newR); err != nil {
		return Hunk{}, fmt.Errorf("hunk header %q: %w", line, err)
	}
	return h, nil
}

// parseRange parses "start,count" or "start" (count 1).
func parseRange(s string) (start, count int, err error) {
	a, b, hasCount := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil || start < 0 {
		return 0, 0, fmt.Errorf("bad range %q", s)
	}
	count = 1
	if hasCount {
		if count, err = strconv.Atoi(b); err != nil || count < 0 {
			return 0, 0, fmt.Errorf("bad range %q", s)
		}
	}
	return start, count, nil
}

// splitGitHeader splits the paths of a "diff --git a/x b/y" line. Paths may be
// quoted. For unquoted paths containing spaces the split is ambiguous; the
// ---/+++ or rename lines, parsed later, give the exact paths.
func splitGitHeader(s string) (oldPath, newPath string, err error) {
	if strings.HasPrefix(s, `"`) {
		end := closingQuote(s)
		if end < 0 {
			return "", "", fmt.Errorf("unterminated quoted path in %q", s)
		}
		oldPath, s = s[:end+1], strings.TrimPrefix(s[end+1:], " ")
	} else {
		// Prefer the split where both halves name the same file, which is
		// right for every unrenamed path, spaces or not.
		i := len(s) / 2
		if i > 2 && i+3 <= len(s) && s[i] == ' ' && s[2:i] == s[i+3:] {
			oldPath, s = s[:i], s[i+1:]
		} else if j := strings.LastIndex(s, ` "b/`); j >= 0 && closingQuote(s[j+1:]) == len(s)-j-2 {
			// An unquoted source renamed to a path git had to quote.
			oldPath, s = s[:j], s[j+1:]
		} else if j := strings.Index(s, " b/"); j >= 0 {
			oldPath, s = s[:j], s[j+1:]
		} else {
			return "", "", fmt.Errorf("cannot split paths in %q", s)
		}
	}
	oldPath, okA := stripPrefix(oldPath, "a/")
	newPath, okB := stripPrefix(s, "b/")
	if !okA || !okB {
		return "", "", fmt.Errorf("paths in %q lack a/ and b/ prefixes", s)
	}
	return oldPath, newPath, nil
}

// stripPrefix unquotes a path and removes its a/ or b/ prefix. It reports
// false for /dev/null or a path without the prefix.
func stripPrefix(path, prefix string) (string, bool) {
	// git appends a tab after paths containing spaces in ---/+++ lines.
	path = unquotePath(strings.TrimSuffix(path, "\t"))
	if rest, ok := strings.CutPrefix(path, prefix); ok {
		return rest, true
	}
	return "", false
}

// unquotePath undoes git's C-style quoting ("a/caf\303\251.go").
func unquotePath(s string) string {
	if len(s) < 2 || s[0] != '"' || closingQuote(s) != len(s)-1 {
		return s
	}
	var b strings.Builder
	for i := 1; i < len(s)-1; i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s)-1 {
			b.WriteByte(c)
			continue
		}
		i++
		switch e := s[i]; e {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '0', '1', '2', '3':
			if i+2 < len(s)-1 {
				if n, err := strconv.ParseUint(s[i:i+3], 8, 8); err == nil {
					b.WriteByte(byte(n))
					i += 2
					continue
				}
			}
			b.WriteByte(e)
		default: // \\ and \"
			b.WriteByte(e)
		}
	}
	return b.String()
}

// closingQuote returns the index of the quote closing the quoted string that
// starts s, or -1.
func closingQuote(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// trimEOL drops a line's newline. A carriage return is kept: in a hunk it is
// part of the file's content.
func trimEOL(s string) string { return strings.TrimSuffix(s, "\n") }

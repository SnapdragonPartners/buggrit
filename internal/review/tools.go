package review

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/glob"
)

// Output caps keep one tool result from flooding the model's context. Each
// truncation says so, and says how to get the rest.
const (
	maxReadLines   = 2000
	maxResultBytes = 200 << 10
	maxGrepMatches = 200
	maxGrepFile    = 1 << 20
	maxLineChars   = 300
	// maxReadLineBytes caps one line of read_file or get_diff output, so a
	// minified file can't blow past maxResultBytes in a single line.
	maxReadLineBytes = 4 << 10
	maxDirEntries    = 1000
)

// toolEnv is what the read-only tools work over.
type toolEnv struct {
	ws   Workspace
	diff *github.Diff
}

// readOnlyTools returns read_file, grep, list_dir and get_diff (design §5).
func readOnlyTools(env *toolEnv) []toolloop.Tool {
	return []toolloop.Tool{
		{Definition: readFileDef(), Execute: env.readFile},
		{Definition: grepDef(), Execute: env.grep},
		{Definition: listDirDef(), Execute: env.listDir},
		{Definition: getDiffDef(), Execute: env.getDiff},
	}
}

// toolError is a model-visible failure: the model sees the message and can
// try again.
func toolError(format string, args ...any) (toolloop.ToolResult, error) {
	return toolloop.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}, nil
}

func ok(content string) (toolloop.ToolResult, error) {
	return toolloop.ToolResult{Content: content}, nil
}

// decodeArgs unmarshals a call's parameters strictly, so a misspelled field
// is reported instead of silently ignored.
func decodeArgs(call *llms.ToolCall, v any) error {
	params := call.Parameters
	if len(bytes.TrimSpace(params)) == 0 {
		params = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments for %s: %w", call.Name, err)
	}
	return nil
}

func readFileDef() llms.ToolDefinition {
	return llms.ToolDefinition{
		Name: "read_file",
		Description: "Read a file from the repository at the reviewed commit. Lines are numbered " +
			"(\"  42| text\"); use those numbers in findings. Reads at most 2000 lines per call; " +
			"pass start_line and end_line to read a window of a long file.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Path relative to the repository root"},
    "start_line": {"type": "integer", "minimum": 1, "description": "First line to read (default 1)"},
    "end_line": {"type": "integer", "minimum": 1, "description": "Last line to read, inclusive"}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
	}
}

//nolint:gocritic // hugeParam: the signature is fixed by toolloop.Tool.Execute
func (e *toolEnv) readFile(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
	var args struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := decodeArgs(&call, &args); err != nil {
		return toolError("%v", err)
	}
	name, err := cleanPath(args.Path)
	if err != nil {
		return toolError("%v", err)
	}
	data, err := readFile(e.ws, name)
	if err != nil {
		return toolError("read %s: %v", name, describeFSError(err))
	}
	if isBinary(data) {
		return toolError("%s is a binary file", name)
	}
	lines := splitLines(data)
	start := max(args.StartLine, 1)
	end := args.EndLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if len(lines) == 0 {
		return ok(name + " is empty")
	}
	if start > len(lines) {
		return toolError("%s has %d lines; start_line %d is past the end", name, len(lines), start)
	}
	if start > end {
		return toolError("start_line %d is after end_line %d", start, end)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s (lines %d-%d of %d)\n", name, start, end, len(lines))
	last := start - 1
	for i := start; i <= end; i++ {
		if i-start >= maxReadLines || b.Len() > maxResultBytes {
			break
		}
		fmt.Fprintf(&b, "%6d| %s\n", i, clipLine(lines[i-1]))
		last = i
	}
	if last < end {
		fmt.Fprintf(&b, "[truncated at line %d; call read_file with start_line=%d to continue]\n", last, last+1)
	}
	return ok(b.String())
}

// splitLines splits file content into lines without their terminators. A
// final newline doesn't start an extra empty line.
func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(data), "\n")
	return strings.Split(s, "\n")
}

// describeFSError turns an fs error into a short, model-readable reason
// without the host's absolute paths.
func describeFSError(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "no such file or directory"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func grepDef() llms.ToolDefinition {
	return llms.ToolDefinition{
		Name: "grep",
		Description: "Search repository files for a regular expression (RE2 syntax, as in Go). " +
			"Returns \"path:line: text\" for each match, up to 200 matches. Use path_glob to narrow " +
			"the search, for example \"internal/**/*.go\" or \"docs/**\".",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "RE2 regular expression"},
    "path_glob": {"type": "string", "description": "Only search paths matching this glob; ** matches any number of directories (default: all files)"},
    "ignore_case": {"type": "boolean"}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`),
	}
}

//nolint:gocritic // hugeParam: the signature is fixed by toolloop.Tool.Execute
func (e *toolEnv) grep(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
	var args struct {
		Pattern    string `json:"pattern"`
		PathGlob   string `json:"path_glob"`
		IgnoreCase bool   `json:"ignore_case"`
	}
	if err := decodeArgs(&call, &args); err != nil {
		return toolError("%v", err)
	}
	expr := args.Pattern
	if args.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return toolError("bad pattern: %v", err)
	}
	pathGlob := strings.TrimPrefix(strings.TrimSpace(args.PathGlob), "./")
	if pathGlob == "" {
		pathGlob = "**"
	}
	if err := glob.Validate(pathGlob); err != nil {
		return toolError("bad path_glob: %v", err)
	}

	g := &grepper{ws: e.ws, re: re, glob: pathGlob}
	walkErr := fs.WalkDir(e.ws, ".", func(name string, d fs.DirEntry, werr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr //nolint:wrapcheck // returned to WalkDir, wrapped below
		}
		return g.visit(name, d, werr)
	})
	if walkErr != nil {
		return toolloop.ToolResult{}, fmt.Errorf("grep: %w", walkErr)
	}
	if g.matches == 0 {
		return ok("no matches")
	}
	if g.truncated {
		fmt.Fprintf(&g.out, "[stopped after %d matches; narrow pattern or path_glob]\n", g.matches)
	}
	return ok(g.out.String())
}

// grepper accumulates grep matches across a directory walk.
type grepper struct {
	ws        Workspace
	re        *regexp.Regexp
	glob      string
	out       strings.Builder
	matches   int
	truncated bool
}

// visit is the WalkDir callback. Unreadable entries, .git, symlinks, large
// files and binary files are skipped; the search goes on.
func (g *grepper) visit(name string, d fs.DirEntry, err error) error {
	switch {
	case err != nil:
		return nil //nolint:nilerr // skip unreadable entries
	case name != "." && hiddenPath(name):
		// .git as a directory, or as a gitfile in a submodule.
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	case d.IsDir():
		return nil
	case !d.Type().IsRegular() || !glob.Match(g.glob, name):
		return nil
	}
	if info, ierr := d.Info(); ierr != nil || info.Size() > maxGrepFile {
		return nil //nolint:nilerr // large or unreadable files aren't searched
	}
	data, rerr := fs.ReadFile(g.ws, name)
	if rerr != nil || isBinary(data) {
		return nil //nolint:nilerr // unreadable or binary files aren't searched
	}
	return g.search(name, data)
}

// search appends data's matching lines, stopping the walk at the cap.
func (g *grepper) search(name string, data []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxGrepFile)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if !g.re.MatchString(line) {
			continue
		}
		if g.matches >= maxGrepMatches || g.out.Len() > maxResultBytes {
			g.truncated = true
			return fs.SkipAll
		}
		fmt.Fprintf(&g.out, "%s:%d: %s\n", name, n, clip(line))
		g.matches++
	}
	return nil
}

func clip(line string) string { return truncate(line, maxLineChars) }

// clipLine bounds one line of file or diff output, saying how long it was.
func clipLine(line string) string {
	if len(line) <= maxReadLineBytes {
		return line
	}
	return truncate(line, maxReadLineBytes) + fmt.Sprintf(" [line is %d bytes; clipped]", len(line))
}

func listDirDef() llms.ToolDefinition {
	return llms.ToolDefinition{
		Name:        "list_dir",
		Description: "List a repository directory. Directories end in \"/\". Defaults to the repository root.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory relative to the repository root (default: the root)"}
  },
  "additionalProperties": false
}`),
	}
}

//nolint:gocritic // hugeParam: the signature is fixed by toolloop.Tool.Execute
func (e *toolEnv) listDir(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(&call, &args); err != nil {
		return toolError("%v", err)
	}
	name, err := cleanPath(args.Path)
	if err != nil {
		return toolError("%v", err)
	}
	entries, err := readDir(e.ws, name)
	if err != nil {
		return toolError("list %s: %v", name, describeFSError(err))
	}
	var b strings.Builder
	shown := 0
	for _, d := range entries {
		if d.Name() == gitDir {
			continue
		}
		if shown >= maxDirEntries {
			fmt.Fprintf(&b, "[%d more entries not shown]\n", len(entries)-shown)
			break
		}
		b.WriteString(d.Name())
		if d.IsDir() {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
		shown++
	}
	if shown == 0 {
		return ok(name + " is empty")
	}
	return ok(b.String())
}

func getDiffDef() llms.ToolDefinition {
	return llms.ToolDefinition{
		Name: "get_diff",
		Description: "Show the pull request's diff for one file, with head-side line numbers. " +
			"Without a path, list every changed file with its status and line counts.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "A changed file (head path; base path for a deleted file)"},
    "offset": {"type": "integer", "minimum": 0, "description": "Skip this many diff lines, to continue a truncated diff"}
  },
  "additionalProperties": false
}`),
	}
}

//nolint:gocritic // hugeParam: the signature is fixed by toolloop.Tool.Execute
func (e *toolEnv) getDiff(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
	}
	if err := decodeArgs(&call, &args); err != nil {
		return toolError("%v", err)
	}
	if args.Offset < 0 {
		return toolError("offset must not be negative")
	}
	p := strings.TrimPrefix(strings.TrimSpace(args.Path), "./")
	if p == "" {
		return ok(fileList(e.diff, nil))
	}
	f := e.diff.File(p)
	if f == nil {
		return toolError("%s is not changed by this pull request; call get_diff without a path to list changed files", p)
	}
	return ok(renderFileDiff(f, args.Offset, maxResultBytes))
}

// fileList renders the changed files, one per line. If inScope is non-nil,
// files it rejects are marked as not reviewed.
func fileList(d *github.Diff, inScope func(string) bool) string {
	var b strings.Builder
	for i := range d.Files {
		f := &d.Files[i]
		added, deleted := lineCounts(f)
		name := f.Path()
		if f.Status == github.StatusRenamed {
			name = f.OldPath + " -> " + f.NewPath
		}
		fmt.Fprintf(&b, "%s (%s, +%d -%d", name, f.Status, added, deleted)
		if f.Binary {
			b.WriteString(", binary")
		}
		if inScope != nil && !inScope(f.Path()) {
			b.WriteString(", excluded from review")
		}
		b.WriteString(")\n")
	}
	if b.Len() == 0 {
		return "no files changed\n"
	}
	return b.String()
}

func lineCounts(f *github.FileDiff) (added, deleted int) {
	for i := range f.Hunks {
		for j := range f.Hunks[i].Lines {
			switch f.Hunks[i].Lines[j].Kind {
			case github.LineAdded:
				added++
			case github.LineDeleted:
				deleted++
			case github.LineContext:
			}
		}
	}
	return added, deleted
}

// renderFileDiff renders one file's diff for the model, skipping its first
// offset diff lines and stopping near limit bytes with a continuation hint.
// Each head-side line carries its line number, which is what findings anchor
// to:
//
//	@@ -10,3 +10,4 @@
//	    10   ctx line
//	    11 + added line
//	       - deleted line
func renderFileDiff(f *github.FileDiff, offset, limit int) string {
	var b strings.Builder
	header := f.Path()
	switch f.Status {
	case github.StatusRenamed:
		header = f.OldPath + " -> " + f.NewPath
	case github.StatusAdded, github.StatusDeleted:
		header += " (" + string(f.Status) + ")"
	case github.StatusModified:
	}
	fmt.Fprintf(&b, "=== %s\n", header)
	if f.Binary {
		b.WriteString("(binary file)\n")
		return b.String()
	}
	if len(f.Hunks) == 0 {
		b.WriteString("(no content changes)\n")
		return b.String()
	}
	if offset > 0 {
		fmt.Fprintf(&b, "(continuing after %d diff lines)\n", offset)
	}
	n := 0 // diff lines seen, across hunks
	for hi := range f.Hunks {
		h := &f.Hunks[hi]
		if n+len(h.Lines) <= offset {
			n += len(h.Lines)
			continue
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		for li := range h.Lines {
			if n < offset {
				n++
				continue
			}
			if b.Len() > limit {
				fmt.Fprintf(&b, "[diff truncated; call get_diff with path %q and offset %d to continue]\n", f.Path(), n)
				return b.String()
			}
			l := &h.Lines[li]
			num := "      "
			if l.Kind != github.LineDeleted {
				num = fmt.Sprintf("%6d", l.New)
			}
			fmt.Fprintf(&b, "%s %c %s\n", num, byte(l.Kind), clipLine(l.Text))
			n++
		}
	}
	return b.String()
}

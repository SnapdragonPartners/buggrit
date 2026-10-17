package review

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// categories returns the categories a finding may have (design §5).
func categories() []string {
	return []string{
		"correctness", "security", "concurrency", "error-handling", "performance",
		"api-contract", "tests", "docs-drift", "maintainability", "style",
	}
}

// Location is one more place a class finding applies (design §7 rule 1).
type Location struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// Finding is one issue the finder reports, in the shape of submit_findings'
// arguments (design §5). Line 0 means the finding is about the whole file.
type Finding struct {
	Path       string     `json:"path"`
	Side       string     `json:"side,omitempty"`
	Severity   string     `json:"severity"`
	Category   string     `json:"category"`
	Confidence string     `json:"confidence"`
	Title      string     `json:"title"`
	Problem    string     `json:"problem"`
	Impact     string     `json:"impact,omitempty"`
	Fix        string     `json:"fix"`
	Suggestion string     `json:"suggestion,omitempty"`
	Class      string     `json:"class,omitempty"`
	Instances  []Location `json:"instances,omitempty"`
	StartLine  int        `json:"start_line,omitempty"`
	Line       int        `json:"line"`
}

// Blocking reports whether the finding blocks convergence (P0 or P1, §5).
func (f *Finding) Blocking() bool {
	return f.Severity == config.SeverityP0 || f.Severity == config.SeverityP1
}

// Coverage values: whether the model reviewed the whole change.
const (
	CoverageComplete = "complete"
	CoveragePartial  = "partial"
)

// Submission is submit_findings' argument object. Coverage is the model's
// own account of whether it reviewed the whole change; "partial" leaves the
// pass incomplete, so the next run reviews it again (design §9).
type Submission struct {
	Summary  string    `json:"summary,omitempty"`
	Coverage string    `json:"coverage"`
	Findings []Finding `json:"findings"`
}

const maxTitle = 120

func submitFindingsDef() llms.ToolDefinition {
	return llms.ToolDefinition{
		Name: "submit_findings",
		Description: "Submit every finding from this review, all at once, and end the review. " +
			"Call it exactly once, after investigating. An empty findings list means the change " +
			"has no issues worth raising. If the submission is invalid you get an error listing " +
			"each problem; fix them all and call submit_findings again.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "description": "One short paragraph on the change overall; with partial coverage, say what you did not get to"},
    "coverage": {"type": "string", "enum": ["complete", "partial"], "description": "complete if you reviewed the whole change; partial if you ran short of time or could not cover some of it"},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "A file changed by this pull request; on a later pass, one changed since the last completed review (see the opening message's scope section)"},
          "line": {"type": "integer", "minimum": 0, "description": "Head-side line the finding anchors to (the last line of a range); 0 for the whole file"},
          "start_line": {"type": "integer", "minimum": 1, "description": "First line of a multi-line range (optional)"},
          "side": {"type": "string", "enum": ["RIGHT", "LEFT"], "description": "RIGHT (default) for head lines; LEFT only for a deleted line, numbered in the base file"},
          "severity": {"type": "string", "enum": ["P0", "P1", "P2", "P3"]},
          "category": {"type": "string", "enum": ["correctness", "security", "concurrency", "error-handling", "performance", "api-contract", "tests", "docs-drift", "maintainability", "style"]},
          "confidence": {"type": "string", "enum": ["low", "medium", "high"]},
          "title": {"type": "string", "description": "One line, at most 120 characters"},
          "problem": {"type": "string", "description": "What is wrong and why, citing the code"},
          "impact": {"type": "string", "description": "What goes wrong for users or callers (optional)"},
          "fix": {"type": "string", "description": "How to fix it; for a class, the one place to fix every instance"},
          "suggestion": {"type": "string", "description": "Exact replacement text for the anchored head lines (optional; RIGHT side only)"},
          "class": {"type": "string", "description": "Short name of the pattern when this finding covers several instances"},
          "instances": {
            "type": "array",
            "description": "Every other place the same class occurs",
            "items": {
              "type": "object",
              "properties": {"path": {"type": "string"}, "line": {"type": "integer", "minimum": 0}},
              "required": ["path", "line"],
              "additionalProperties": false
            }
          }
        },
        "required": ["path", "line", "severity", "category", "confidence", "title", "problem", "fix"],
        "additionalProperties": false
      }
    }
  },
  "required": ["coverage", "findings"],
  "additionalProperties": false
}`),
	}
}

// submitTool is the terminal tool. A valid submission stops the loop; an
// invalid one is returned to the model with every problem listed, so it can
// fix them all in one resubmission (design §5).
type submitTool struct {
	env     *toolEnv
	inScope func(string) bool
	// incremental, if set, is where this pass may anchor findings.
	incremental *Incremental
	result      *Submission
}

func (s *submitTool) tool() toolloop.Tool {
	return toolloop.Tool{Definition: submitFindingsDef(), Execute: s.execute}
}

//nolint:gocritic // hugeParam: the signature is fixed by toolloop.Tool.Execute
func (s *submitTool) execute(_ context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
	var sub Submission
	if err := decodeArgs(&call, &sub); err != nil {
		return toolError("%v. Fix the arguments and call submit_findings again.", err)
	}
	if sub.Findings == nil {
		return toolError("findings is required; pass [] if there are none. Call submit_findings again.")
	}
	sub.Coverage = strings.ToLower(strings.TrimSpace(sub.Coverage))
	if sub.Coverage != CoverageComplete && sub.Coverage != CoveragePartial {
		return toolError("coverage must be %q or %q, not %q. Call submit_findings again.", CoverageComplete, CoveragePartial, sub.Coverage)
	}
	missing := missingLocationFields(call.Parameters)
	var problems []string
	for i := range sub.Findings {
		f := &sub.Findings[i]
		normalize(f)
		// Every field problem is reported at once (§5). Only the anchor
		// checks are skipped for a missing location: on a defaulted line
		// they would be misleading.
		ps := append(missing[i], s.validateFields(f)...)
		if len(missing[i]) == 0 {
			ps = append(ps, s.validateLocations(f)...)
		}
		for _, p := range ps {
			problems = append(problems, fmt.Sprintf("findings[%d] (%s): %s", i, f.Path, p))
		}
	}
	if len(problems) > 0 {
		return toolError("submit_findings was not accepted. Fix every problem below and call it again with the complete list:\n- %s",
			strings.Join(problems, "\n- "))
	}
	s.result = &sub
	return toolloop.ToolResult{
		Content: fmt.Sprintf("accepted %d findings", len(sub.Findings)),
		Stop:    true,
	}, nil
}

// missingLocationFields reports, per finding, location fields that are
// absent or null. Decoding turns those into "" and 0, and line 0 is a valid
// file-level anchor, so presence has to be checked on the raw arguments; an
// explicit "line": 0 stays allowed. params already decoded strictly, so this
// second decode can't fail.
func missingLocationFields(params json.RawMessage) map[int][]string {
	var raw struct {
		Findings []struct {
			Path      json.RawMessage `json:"path"`
			Line      json.RawMessage `json:"line"`
			Instances []struct {
				Path json.RawMessage `json:"path"`
				Line json.RawMessage `json:"line"`
			} `json:"instances"`
		} `json:"findings"`
	}
	_ = json.Unmarshal(params, &raw)
	absent := func(v json.RawMessage) bool { return len(v) == 0 || string(v) == "null" }
	out := map[int][]string{}
	for i := range raw.Findings {
		f := &raw.Findings[i]
		if absent(f.Path) {
			out[i] = append(out[i], "path is required")
		}
		if absent(f.Line) {
			out[i] = append(out[i], "line is required; use 0 only for a finding about the whole file")
		}
		for j := range f.Instances {
			loc := &f.Instances[j]
			if absent(loc.Path) {
				out[i] = append(out[i], fmt.Sprintf("instances[%d]: path is required", j))
			}
			if absent(loc.Line) {
				out[i] = append(out[i], fmt.Sprintf("instances[%d]: line is required; use 0 for the whole file", j))
			}
		}
	}
	return out
}

// normalize trims fields and fills defaults before validation.
func normalize(f *Finding) {
	for _, p := range []*string{&f.Path, &f.Side, &f.Severity, &f.Category, &f.Confidence, &f.Title, &f.Class} {
		*p = strings.TrimSpace(*p)
	}
	f.Path = strings.TrimPrefix(f.Path, "./")
	f.Side = strings.ToUpper(f.Side)
	if f.Side == "" {
		f.Side = string(github.SideRight)
	}
	f.Severity = strings.ToUpper(f.Severity)
	f.Category = strings.ToLower(f.Category)
	f.Confidence = strings.ToLower(f.Confidence)
	if f.StartLine == f.Line {
		f.StartLine = 0
	}
	for i := range f.Instances {
		f.Instances[i].Path = strings.TrimPrefix(strings.TrimSpace(f.Instances[i].Path), "./")
	}
}

// validateFields returns the problems with a finding's fields other than
// its locations, phrased for the model.
func (s *submitTool) validateFields(f *Finding) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	if _, ok := config.SeverityRank(f.Severity); !ok {
		add("severity must be P0, P1, P2 or P3, not %q", f.Severity)
	}
	if !slices.Contains(categories(), f.Category) {
		add("category must be one of %s, not %q", strings.Join(categories(), ", "), f.Category)
	}
	switch f.Confidence {
	case config.ConfidenceLow, config.ConfidenceMedium, config.ConfidenceHigh:
	default:
		add("confidence must be low, medium or high, not %q", f.Confidence)
	}
	switch {
	case f.Title == "":
		add("title is required")
	case len(f.Title) > maxTitle || strings.Contains(f.Title, "\n"):
		add("title must be one line of at most %d characters", maxTitle)
	}
	if strings.TrimSpace(f.Problem) == "" {
		add("problem is required")
	}
	if strings.TrimSpace(f.Fix) == "" {
		add("fix is required")
	}
	if len(f.Instances) > 0 && f.Class == "" {
		add("instances need a class naming the shared pattern")
	}
	return out
}

// validateLocations checks the finding's anchor and its instances.
func (s *submitTool) validateLocations(f *Finding) []string {
	out := s.validateAnchor(f)
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	for j, loc := range f.Instances {
		if p := s.checkLine(loc.Path, loc.Line); p != "" {
			add("instances[%d]: %s", j, p)
		}
	}
	return out
}

// validateAnchor checks that the finding's path and lines can be anchored.
func (s *submitTool) validateAnchor(f *Finding) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	fd := s.env.diff.File(f.Path)
	switch {
	case fd == nil:
		add("%s is not changed by this pull request. Anchor the finding to the changed line that causes it, "+
			"and name %s in problem or instances", f.Path, f.Path)
		return out
	case !s.inScope(f.Path):
		add("%s is excluded from review by the repository's configuration", f.Path)
		return out
	}
	if f.Line < 0 || f.StartLine < 0 {
		add("line numbers must not be negative")
		return out
	}
	if f.StartLine > f.Line {
		add("start_line %d is after line %d", f.StartLine, f.Line)
	}

	switch github.Side(f.Side) {
	case github.SideLeft:
		out = append(out, leftProblems(f, fd)...)
	case github.SideRight:
		out = append(out, s.rightProblems(f, fd)...)
	default:
		add("side must be RIGHT or LEFT, not %q", f.Side)
	}
	if len(out) == 0 {
		// The scope check reads the anchor, so on an invalid one it would
		// only add a misleading second problem.
		if p := s.incremental.check(f, anchor(f, s.env.diff)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// leftProblems checks a finding on a deleted (base-side) line.
func leftProblems(f *Finding, fd *github.FileDiff) []string {
	var out []string
	if f.Line == 0 || !fd.Commentable(f.Line, github.SideLeft) {
		out = append(out, "side LEFT needs line to be a deleted line in the diff, numbered in the base file")
	}
	if f.Suggestion != "" {
		out = append(out, "suggestion is only allowed on RIGHT-side lines")
	}
	return out
}

// rightProblems checks a finding on head-side lines.
func (s *submitTool) rightProblems(f *Finding, fd *github.FileDiff) []string {
	var out []string
	switch {
	case fd.Status == github.StatusDeleted && f.Line == 0:
		// A file-level finding about the deletion itself.
	case fd.Status == github.StatusDeleted:
		out = append(out, fmt.Sprintf("%s is deleted; use side LEFT with a deleted line number, or line 0", f.Path))
	case f.Line > 0 && fd.Commentable(f.Line, github.SideRight):
		// The diff shows this head line, so it exists; no read needed. This
		// also covers a changed symlink, whose content the tools won't read.
	default:
		if p := s.checkLine(f.Path, f.Line); p != "" {
			out = append(out, p)
		}
	}
	if f.Suggestion != "" && f.Line == 0 {
		out = append(out, "suggestion needs a line to replace")
	}
	return out
}

// checkLine reports why line can't be a head-side line of path, or "".
func (s *submitTool) checkLine(p string, line int) string {
	name, err := cleanPath(p)
	if err != nil {
		return err.Error()
	}
	if line == 0 {
		// File level: the entry must exist, but it may be a symlink, so
		// check it without following it.
		if lerr := checkNoSymlink(s.env.ws, path.Dir(name)); lerr != nil {
			return lerr.Error()
		}
		if _, serr := s.env.ws.Lstat(name); serr != nil {
			return fmt.Sprintf("%s: %s", name, describeFSError(serr))
		}
		return ""
	}
	if line < 0 {
		return "line must not be negative"
	}
	// The scan stops at line; if it runs out first, n is the line count.
	n, err := scanLines(s.env.ws, name, func(n int, _ []byte, _ int) bool { return n < line })
	if err != nil {
		return fmt.Sprintf("%s: %s", name, describeFSError(err))
	}
	if line > n {
		return fmt.Sprintf("%s has %d lines; line %d does not exist", name, n, line)
	}
	return ""
}

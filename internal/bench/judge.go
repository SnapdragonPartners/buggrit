package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Verdicts on a known defect, and on an extra finding.
const (
	Found   = "found"
	Partial = "partial"
	Missed  = "missed"
	Real    = "real"
	False   = "false"
)

// RunScore is a run's judgment: a verdict on every known defect, and on
// every finding that matched none.
type RunScore struct {
	RunID   string
	Defects []DefectScore
	Extras  []ExtraScore
}

// DefectScore is one defect's verdict in one run. The judge's verdict is a
// draft; a person's Override replaces it.
type DefectScore struct {
	Defect    string `json:"defect"`
	Verdict   string `json:"verdict"`
	Reasoning string `json:"reasoning"`
	Override  string `json:"-"`
	Note      string `json:"-"`
	// Finding is the 1-based number of the matching finding in the run's
	// kept findings; 0 for none.
	Finding int `json:"finding"`
}

// Effective is the verdict that counts: the override, if there is one.
func (d *DefectScore) Effective() string {
	if d.Override != "" {
		return d.Override
	}
	return d.Verdict
}

// ExtraScore is the verdict on a finding that matched no known defect.
type ExtraScore struct {
	Verdict   string `json:"verdict"`
	Reasoning string `json:"reasoning"`
	Override  string `json:"-"`
	Note      string `json:"-"`
	Finding   int    `json:"finding"`
}

// Effective is the verdict that counts: the override, if there is one.
func (e *ExtraScore) Effective() string {
	if e.Override != "" {
		return e.Override
	}
	return e.Verdict
}

// Credit is a defect verdict's share of recall: 1 found, ½ partial.
func Credit(verdict string) float64 {
	switch verdict {
	case Found:
		return 1
	case Partial:
		return 0.5
	}
	return 0
}

// Recall is the run's credit over the benchmark's defects.
func (s *RunScore) Recall() float64 {
	if len(s.Defects) == 0 {
		return 0
	}
	var sum float64
	for i := range s.Defects {
		sum += Credit(s.Defects[i].Effective())
	}
	return sum / float64(len(s.Defects))
}

// Judge scores a run's findings against a benchmark with one model call:
// every finding against every defect, with the findings whose location
// overlaps a defect's marked as hints, not used as a filter (a defect can
// be reported where it shows rather than where it is). The judgment comes
// back through a tool call; an invalid one gets the problems back,
// up to MaxAttempts calls, as
// does a response that doesn't call the tool.
type Judge struct {
	Chat        llms.ChatClient
	MaxAttempts int
}

const judgeTool = "submit_judgment"

const judgeSystem = `You score an automated code reviewer's findings against a benchmark: a list of known defects in one change, each raised earlier by another reviewer and later fixed.

For every known defect, decide whether one of the findings reports it:
- found: a finding identifies this defect, the same underlying problem, even if it's reported at a different line or file than the benchmark lists, or worded differently.
- partial: a finding points at the defect's code but misreads the problem, or its fix wouldn't fix it.
- missed: no finding reports it.
A defect that's found or partial names the one finding that best matches it. A finding may match more than one defect.

Then, for every finding that matches no defect (an "extra" finding), give a draft verdict:
- real: from its description, a genuine defect worth raising.
- false: wrong, a misreading, or not a defect.
You can't read the code, so judge extras on the finding's own reasoning; a person reviews these verdicts.

Give one line of reasoning for each verdict. Submit everything with one call to submit_judgment.`

func judgeToolDef() llms.ToolDefinition {
	return llms.ToolDefinition{
		Name:        judgeTool,
		Description: "Submit the judgment: a verdict on every known defect and on every extra finding.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "defects": {
      "type": "array",
      "description": "One entry per known defect, each defect exactly once",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "description": "The defect's id"},
          "verdict": {"type": "string", "enum": ["found", "partial", "missed"]},
          "finding": {"type": "integer", "description": "The number of the matching finding; 0 when missed"},
          "reasoning": {"type": "string", "description": "One line"}
        },
        "required": ["id", "verdict", "finding", "reasoning"]
      }
    },
    "extras": {
      "type": "array",
      "description": "One entry per finding that matches no defect, each exactly once",
      "items": {
        "type": "object",
        "properties": {
          "finding": {"type": "integer", "description": "The finding's number"},
          "verdict": {"type": "string", "enum": ["real", "false"]},
          "reasoning": {"type": "string", "description": "One line"}
        },
        "required": ["finding", "verdict", "reasoning"]
      }
    }
  },
  "required": ["defects", "extras"]
}`),
	}
}

type judgment struct {
	Defects []struct {
		ID        string `json:"id"`
		Verdict   string `json:"verdict"`
		Reasoning string `json:"reasoning"`
		Finding   int    `json:"finding"`
	} `json:"defects"`
	Extras []ExtraScore `json:"extras"`
}

// DefaultJudgeAttempts is how many calls a judgment gets when MaxAttempts
// isn't set.
const DefaultJudgeAttempts = 3

// Score judges one run. A run with no findings misses every defect without
// a model call.
func (j *Judge) Score(ctx context.Context, b *Benchmark, run *RunResult) (*RunScore, error) {
	if len(run.Findings) == 0 {
		s := &RunScore{RunID: run.ID}
		for i := range b.Defects {
			s.Defects = append(s.Defects, DefectScore{Defect: b.Defects[i].ID, Verdict: Missed, Reasoning: "the run kept no findings"})
		}
		return s, nil
	}
	attempts := j.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultJudgeAttempts
	}
	msgs := []llms.Message{llms.UserText(JudgePrompt(b, run.Findings))}
	var last error
	for range attempts {
		resp, err := j.Chat.Complete(ctx, llms.ChatRequest{
			System:   []llms.ContentPart{llms.Text(judgeSystem)},
			Messages: msgs,
			Tools:    []llms.ToolDefinition{judgeToolDef()},
			// Auto, not forced: some providers refuse a forced tool with
			// reasoning on. A response without the call gets a nudge.
			ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceAuto},
			Purpose:    "bench_judge",
		})
		if err != nil {
			return nil, fmt.Errorf("judge run %s: %w", run.ID, err)
		}
		call, ok := toolCall(&resp.Message)
		if !ok {
			last = errors.New("the response didn't call " + judgeTool)
			msgs = append(msgs, resp.Message, llms.UserText("Call "+judgeTool+" with the judgment."))
			continue
		}
		score, problems := parseJudgment(call.Parameters, b, len(run.Findings))
		if len(problems) == 0 {
			score.RunID = run.ID
			return score, nil
		}
		last = fmt.Errorf("invalid judgment: %s", strings.Join(problems, "; "))
		msgs = append(msgs, resp.Message, llms.ToolResultMessage(llms.ToolResult{
			ToolCallID: call.ID, IsError: true,
			Content: "The judgment is invalid; fix every problem and call " + judgeTool + " again:\n- " + strings.Join(problems, "\n- "),
		}))
	}
	return nil, fmt.Errorf("judge run %s: no valid judgment in %d calls: %w", run.ID, attempts, last)
}

func toolCall(m *llms.Message) (*llms.ToolCall, bool) {
	for i := range m.Content {
		if c := m.Content[i].ToolCall; c != nil && c.Name == judgeTool {
			return c, true
		}
	}
	return nil, false
}

// parseJudgment checks a judgment covers every defect once, and every
// finding no defect matched once as an extra, and nothing else.
func parseJudgment(params json.RawMessage, b *Benchmark, findings int) (*RunScore, []string) {
	var j judgment
	if err := json.Unmarshal(params, &j); err != nil {
		return nil, []string{"the arguments aren't valid JSON for the schema: " + err.Error()}
	}
	s := &RunScore{}
	matched := make([]bool, findings+1)
	var problems []string
	s.Defects, problems = parseDefects(&j, b, findings, matched)
	var extraProblems []string
	s.Extras, extraProblems = parseExtras(&j, findings, matched)
	return s, append(problems, extraProblems...)
}

// parseDefects checks the defect verdicts and marks the findings they
// match. It returns them in the benchmark's order, so scores read the same
// every time.
func parseDefects(j *judgment, b *Benchmark, findings int, matched []bool) ([]DefectScore, []string) {
	var problems []string
	index := map[string]int{}
	for i := range b.Defects {
		index[b.Defects[i].ID] = i
	}
	byID := map[string]bool{}
	out := make([]DefectScore, 0, len(j.Defects))
	for i := range j.Defects {
		d := &j.Defects[i]
		if _, ok := index[d.ID]; !ok {
			problems = append(problems, fmt.Sprintf("defect %q isn't in the benchmark", d.ID))
			continue
		}
		if byID[d.ID] {
			problems = append(problems, fmt.Sprintf("defect %q is judged twice", d.ID))
			continue
		}
		byID[d.ID] = true
		switch d.Verdict {
		case Found, Partial:
			if d.Finding < 1 || d.Finding > findings {
				problems = append(problems, fmt.Sprintf("defect %q is %s but names finding %d, which doesn't exist", d.ID, d.Verdict, d.Finding))
				continue
			}
			matched[d.Finding] = true
		case Missed:
			d.Finding = 0
		default:
			problems = append(problems, fmt.Sprintf("defect %q: verdict %q isn't found, partial or missed", d.ID, d.Verdict))
			continue
		}
		out = append(out, DefectScore{Defect: d.ID, Verdict: d.Verdict, Finding: d.Finding, Reasoning: d.Reasoning})
	}
	for i := range b.Defects {
		if !byID[b.Defects[i].ID] {
			problems = append(problems, fmt.Sprintf("defect %q has no verdict", b.Defects[i].ID))
		}
	}
	slices.SortFunc(out, func(a, c DefectScore) int { return index[a.Defect] - index[c.Defect] })
	return out, problems
}

// parseExtras checks there's one verdict for each finding no defect
// matched, and none for the others.
func parseExtras(j *judgment, findings int, matched []bool) ([]ExtraScore, []string) {
	var problems []string
	extra := make([]bool, findings+1)
	var out []ExtraScore
	for i := range j.Extras {
		e := &j.Extras[i]
		switch {
		case e.Finding < 1 || e.Finding > findings:
			problems = append(problems, fmt.Sprintf("extra finding %d doesn't exist", e.Finding))
		case matched[e.Finding]:
			problems = append(problems, fmt.Sprintf("finding %d matches a defect, so it isn't an extra", e.Finding))
		case extra[e.Finding]:
			problems = append(problems, fmt.Sprintf("extra finding %d is judged twice", e.Finding))
		case e.Verdict != Real && e.Verdict != False:
			problems = append(problems, fmt.Sprintf("extra finding %d: verdict %q isn't real or false", e.Finding, e.Verdict))
		default:
			extra[e.Finding] = true
			out = append(out, ExtraScore{Finding: e.Finding, Verdict: e.Verdict, Reasoning: e.Reasoning})
		}
	}
	for n := 1; n <= findings; n++ {
		if !matched[n] && !extra[n] {
			problems = append(problems, fmt.Sprintf("finding %d matches no defect and has no extra verdict", n))
		}
	}
	slices.SortFunc(out, func(a, c ExtraScore) int { return a.Finding - c.Finding })
	return out, problems
}

// JudgePrompt is the judge's opening message: the change, the known
// defects with the findings whose location overlaps each, and the
// findings.
func JudgePrompt(b *Benchmark, findings []Finding) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "The change: %s\n\n", b.Title)
	sb.WriteString("## Known defects\n\n")
	for i := range b.Defects {
		d := &b.Defects[i]
		locs := make([]string, 0, len(d.Locations))
		for k := range d.Locations {
			locs = append(locs, d.Locations[k].String())
		}
		fmt.Fprintf(&sb, "- **%s** (raised by %s, at %s): %s\n", d.ID, d.Source, strings.Join(locs, ", "), d.Description)
		var near []string
		for n := range findings {
			if d.Overlaps(&findings[n]) {
				near = append(near, fmt.Sprint(n+1))
			}
		}
		if len(near) > 0 {
			fmt.Fprintf(&sb, "  - Findings at an overlapping location (a hint, not a rule): %s\n", strings.Join(near, ", "))
		}
	}
	sb.WriteString("\n## Findings\n")
	for n := range findings {
		f := &findings[n]
		fmt.Fprintf(&sb, "\n### Finding %d: %s\n\n", n+1, f.Title)
		fmt.Fprintf(&sb, "%s · %s · at %s\n", f.Severity, orDash(f.Category), f.Where())
		if f.Class != "" {
			var inst []string
			for _, in := range f.Instances {
				inst = append(inst, fmt.Sprintf("%s:%d", in.Path, in.Line))
			}
			fmt.Fprintf(&sb, "A class finding (%s), also at: %s\n", f.Class, strings.Join(inst, ", "))
		}
		if f.Problem != "" {
			fmt.Fprintf(&sb, "\nProblem: %s\n", f.Problem)
		}
		if f.Fix != "" {
			fmt.Fprintf(&sb, "\nFix: %s\n", f.Fix)
		}
	}
	return sb.String()
}

// String is path:start-end, or the path alone for the whole file.
func (l Location) String() string {
	switch {
	case l.Start == 0:
		return l.Path
	case l.Start == l.End:
		return fmt.Sprintf("%s:%d", l.Path, l.Start)
	}
	return fmt.Sprintf("%s:%d-%d", l.Path, l.Start, l.End)
}

// Where is the finding's path and line, or line range.
func (f *Finding) Where() string {
	switch {
	case f.Line == 0:
		return f.Path
	case f.StartLine > 0 && f.StartLine < f.Line:
		return fmt.Sprintf("%s:%d-%d", f.Path, f.StartLine, f.Line)
	}
	return fmt.Sprintf("%s:%d", f.Path, f.Line)
}

// Overlaps reports whether a finding, or any instance of its class, is at
// one of the defect's locations: the same file, with overlapping lines or
// either side covering the whole file.
func (d *Defect) Overlaps(f *Finding) bool {
	type span struct {
		path       string
		start, end int
	}
	spans := []span{{f.Path, max(f.StartLine, 0), f.Line}}
	if spans[0].start == 0 || spans[0].start > f.Line {
		spans[0].start = f.Line
	}
	for _, in := range f.Instances {
		spans = append(spans, span{in.Path, in.Line, in.Line})
	}
	for i := range d.Locations {
		l := &d.Locations[i]
		for k := range spans {
			s := &spans[k]
			if s.path != l.Path {
				continue
			}
			if l.Start == 0 || s.end == 0 || (s.start <= l.End && l.Start <= s.end) {
				return true
			}
		}
	}
	return false
}

// dash stands for an empty cell or a missing value.
const dash = "–"

func orDash(s string) string {
	if s == "" {
		return dash
	}
	return s
}

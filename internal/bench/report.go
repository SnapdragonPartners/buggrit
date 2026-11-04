package bench

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReportInput is everything a report shows.
type ReportInput struct {
	Experiment *ExperimentRecord
	// Scores are the runs' judgments by run ID, with overrides applied;
	// nil without a benchmark.
	Scores map[string]*RunScore
	// RunURL links the workflow run, when known.
	RunURL string
	// Runs are the experiment's runs.
	Runs []RunResult
}

// armRun is a run with its place in its arm.
type armRun struct {
	run   *RunResult
	score *RunScore
	name  string
}

// groupKey is an arm's runs that share an identity group.
type groupKey struct{ arm, group string }

// report is a report being written.
type report struct {
	e      *ExperimentRecord
	groups map[groupKey][]armRun
	sb     strings.Builder
	runs   []armRun
	keys   []groupKey
	scored bool
}

func (r *report) w(format string, args ...any) { fmt.Fprintf(&r.sb, format, args...) }

// Report renders an experiment's results as Markdown, in the shape of the
// buggrit#8 tables: each run, each arm by identity group, how each known
// defect fared, the extra findings and each run's findings. Averages are
// taken only within an identity group (docs/telemetry.md, "Identity").
func Report(in *ReportInput) string {
	// Scores mean something only against their benchmark, so every scored
	// section can rely on it.
	r := &report{e: in.Experiment, runs: numberRuns(in), scored: in.Scores != nil && in.Experiment.Benchmark != nil}
	r.groups = groupRuns(r.runs)
	r.keys = sortedGroups(r.e.Experiment, r.groups)
	r.header(in.RunURL)
	r.arms()
	r.eachRun()
	r.byArm()
	if r.scored {
		writeDefects(&r.sb, r.e.Benchmark, r.runs)
		writeExtras(&r.sb, r.runs)
	}
	writeFindings(&r.sb, r.runs)
	return r.sb.String()
}

func (r *report) header(runURL string) {
	e := r.e
	t := e.Experiment.Target
	if e.Benchmark != nil {
		t = &e.Benchmark.Target
	}
	r.w("## Benchmark: %s\n\n", e.Experiment.Name)
	if e.Benchmark != nil {
		r.w("Benchmark `%s` (%d known defects, `%s`). ", e.Benchmark.Name, len(e.Benchmark.Defects), shortHash(e.BenchmarkHash))
	}
	r.w("Target: %s `%s...%s`, replayed as draft #%d. ", t.Repo, short(t.Base), short(t.Head), e.Replay.PR)
	r.w("%d run(s) per arm requested.", e.Experiment.Runs)
	if runURL != "" {
		r.w(" [Workflow run](%s).", runURL)
	}
	r.w("\n\n")
	if j := e.Experiment.Judge; j != nil {
		r.w("Judge: `%s` %s%s. **Its verdicts are drafts**: a person confirms or overrides them (docs/benchmarking.md).\n\n",
			j.Model, j.Provider, effortSuffix(j.ReasoningEffort))
	}
	if missing := missingRuns(e.Experiment, r.runs); len(missing) > 0 {
		r.w("> **Missing runs:** %s. A run that failed before recording telemetry, or whose job was canceled, has no row.\n\n", strings.Join(missing, "; "))
	}
}

// arms lists each arm's identity groups: what the runs ran.
func (r *report) arms() {
	r.w("### Arms\n\n| Arm | Ref | Group | Find model | Effort | Prompt | Tools | Commit | Runs |\n|---|---|---|---|---|---|---|---|---|\n")
	for i := range r.keys {
		g := &r.keys[i]
		rs := r.groups[*g]
		id := &rs[0].run.Identity
		model, effort := id.FindModel()
		find := id.MPH.Stages["find"]
		commit := short(id.MPH.Commit)
		if id.MPH.Modified {
			commit += " (modified)"
		}
		r.w("| %s | %s | `%s` | %s | %s | `%s` | `%s` | `%s` | %d |\n", g.arm, refOf(r.e, g.arm), g.group,
			orDash(model), orDash(effort), shortHash(find.PromptHash), shortHash(find.ToolsHash), orDash(commit), len(rs))
	}
}

// tableHead writes a table's header, with the score columns when scored.
func (r *report) tableHead(cols string, n int) {
	r.w("%s", cols)
	if r.scored {
		r.w(" Recall | Extra real / false |")
	}
	r.w("\n|%s", strings.Repeat("---|", n))
	if r.scored {
		r.w("---|---|")
	}
	r.w("\n")
}

// eachRun lists every run. A run whose telemetry wasn't all written has
// its counts marked as lower bounds (≥): some of its call and tool events
// may be missing.
func (r *report) eachRun() {
	r.w("\n### Runs\n\nCounts marked ≥ are lower bounds: that run's telemetry wasn't all written.\n\n")
	r.tableHead("| Run | Wall | Turns | s/turn | Tools/turn | Tokens in / out / reasoning / cached | Ended | Telemetry | Findings (blocking) |", 9)
	for i := range r.runs {
		ar := &r.runs[i]
		run := ar.run
		lb, telemetry := "", "complete"
		if !run.Complete {
			lb, telemetry = "≥", "incomplete"
		}
		r.w("| %s | %s | %s%d | %s | %s | %s%d / %d / %d / %d | %s | %s | %d (%d) |", ar.name, dur(run.Wall()), lb, run.Turns,
			perTurn(run.Wall().Seconds(), run.Turns, 0), perTurn(float64(run.ToolCalls), run.Turns, 1),
			lb, run.Tokens.Input, run.Tokens.Output, run.Tokens.Reasoning, run.Tokens.Cached,
			ended(run), telemetry, len(run.Findings), blocking(run.Findings))
		switch {
		case !r.scored:
		case ar.score == nil:
			r.w(" not scored | |")
		default:
			nReal, nFalse := extraCounts(ar.score)
			r.w(" %s | %d / %d |", recallText(ar.score), nReal, nFalse)
		}
		r.w("\n")
	}
}

// groupStats are an identity group's totals, each over the runs it's
// known for: wall time over runs that have one, the call, tool and token
// counts over runs whose telemetry was all written, and findings over runs
// that recorded a result.
type groupStats struct {
	wall, countedWall, turns, tools, in, out, findings, recall, nReal, nFalse float64
	timed, counted, results, done, scored                                     int
}

func statsOf(rs []armRun) groupStats {
	var st groupStats
	for i := range rs {
		run, score := rs[i].run, rs[i].score
		if w := run.Wall().Seconds(); w > 0 {
			st.wall += w
			st.timed++
		}
		if run.Complete {
			st.counted++
			st.countedWall += run.Wall().Seconds()
			st.turns += float64(run.Turns)
			st.tools += float64(run.ToolCalls)
			st.in += float64(run.Tokens.Input)
			st.out += float64(run.Tokens.Output)
		}
		if run.HasResult {
			st.results++
			st.findings += float64(len(run.Findings))
		}
		if run.Done {
			st.done++
		}
		if score != nil {
			st.scored++
			st.recall += score.Recall()
			re, f := extraCounts(score)
			st.nReal += float64(re)
			st.nFalse += float64(f)
		}
	}
	return st
}

func avg(sum float64, n int, format string) string {
	if n == 0 {
		return dash
	}
	return fmt.Sprintf(format, sum/float64(n))
}

// byArm averages each identity group, and places the groups side by side.
func (r *report) byArm() {
	r.w("\n### By arm\n\nAverages within each identity group; groups side by side. Turns, tools and tokens average only the runs whose telemetry was all written.\n\n")
	r.tableHead("| Arm | Group | Runs | Wall | Turns | s/turn | Tools/turn | Tokens in / out | Finished | Telemetry complete | Findings |", 11)
	for i := range r.keys {
		g := &r.keys[i]
		rs := r.groups[*g]
		st := statsOf(rs)
		wall := dash
		if st.timed > 0 {
			wall = dur(time.Duration(st.wall / float64(st.timed) * float64(time.Second)))
		}
		r.w("| %s | `%s` | %d | %s | %s | %s | %s | %s / %s | %d of %d | %d of %d | %s |", g.arm, g.group, len(rs),
			wall, avg(st.turns, st.counted, "%.1f"), perTurn(st.countedWall, int(st.turns), 0), perTurn(st.tools, int(st.turns), 1),
			avg(st.in, st.counted, "%.0f"), avg(st.out, st.counted, "%.0f"), st.done, len(rs), st.counted, len(rs), avg(st.findings, st.results, "%.1f"))
		switch {
		case !r.scored:
		case st.scored == 0:
			r.w(" | |")
		default:
			k := float64(st.scored)
			defects := len(r.e.Benchmark.Defects)
			r.w(" %.0f%% (%.2f of %d) | %.1f / %.1f |", 100*st.recall/k, st.recall/k*float64(defects), defects, st.nReal/k, st.nFalse/k)
		}
		r.w("\n")
	}
}

func writeDefects(sb *strings.Builder, b *Benchmark, runs []armRun) {
	fmt.Fprintf(sb, "\n### Known defects\n\n✓ found, ½ partial, · missed; an override is marked with *.\n\n| Defect | Source | Description |")
	for i := range runs {
		fmt.Fprintf(sb, " %s |", runs[i].name)
	}
	sb.WriteString("\n|---|---|---|")
	for range runs {
		sb.WriteString("---|")
	}
	sb.WriteString("\n")
	for i := range b.Defects {
		d := &b.Defects[i]
		fmt.Fprintf(sb, "| %s | %s | %s |", d.ID, d.Source, cell(d.Description))
		for j := range runs {
			fmt.Fprintf(sb, " %s |", defectMark(runs[j].score, d.ID))
		}
		sb.WriteString("\n")
	}
	var notes []string
	for j := range runs {
		s := runs[j].score
		if s == nil {
			continue
		}
		for k := range s.Defects {
			ds := &s.Defects[k]
			if ds.Effective() == Missed && ds.Override == "" {
				continue
			}
			line := fmt.Sprintf("- **%s, %s** (%s", runs[j].name, ds.Defect, ds.Effective())
			if ds.Override != "" {
				line += fmt.Sprintf(", overridden from %s", ds.Verdict)
			}
			line += ")"
			if f := findingAt(runs[j].run, ds.Finding); f != nil {
				line += fmt.Sprintf(": finding %d, %s %s", ds.Finding, f.Where(), cell(f.Title))
			}
			line += ". " + cell(ds.Reasoning)
			if ds.Note != "" {
				line += " Override: " + cell(ds.Note)
			}
			notes = append(notes, line)
		}
	}
	if len(notes) > 0 {
		sb.WriteString("\n<details><summary>Matches and the judge's reasoning</summary>\n\n" + strings.Join(notes, "\n") + "\n\n</details>\n")
	}
}

func writeExtras(sb *strings.Builder, runs []armRun) {
	var lines []string
	for j := range runs {
		s := runs[j].score
		if s == nil {
			continue
		}
		for k := range s.Extras {
			x := &s.Extras[k]
			f := findingAt(runs[j].run, x.Finding)
			if f == nil {
				continue
			}
			v := "**" + x.Effective() + "** (draft)"
			if x.Override != "" {
				v = "**" + x.Override + "** (overridden from " + x.Verdict + ")"
			}
			line := fmt.Sprintf("- **%s**, finding %d: %s %s at %s: %s. %s", runs[j].name, x.Finding, f.Severity, cell(f.Title), f.Where(), v, cell(x.Reasoning))
			if x.Note != "" {
				line += " Override: " + cell(x.Note)
			}
			lines = append(lines, line)
		}
	}
	sb.WriteString("\n### Extra findings\n\n")
	if len(lines) == 0 {
		sb.WriteString("None: every finding matched a known defect.\n")
		return
	}
	sb.WriteString(strings.Join(lines, "\n") + "\n")
}

func writeFindings(sb *strings.Builder, runs []armRun) {
	sb.WriteString("\n### Findings by run\n\n")
	for i := range runs {
		r := runs[i].run
		fmt.Fprintf(sb, "<details><summary>%s: %d finding(s), run <code>%s</code></summary>\n\n", runs[i].name, len(r.Findings), r.ID)
		if len(r.Findings) == 0 {
			sb.WriteString("No findings.\n")
		}
		for n := range r.Findings {
			f := &r.Findings[n]
			fmt.Fprintf(sb, "%d. **%s** %s, at `%s`", n+1, f.Severity, cell(f.Title), f.Where())
			if f.Class != "" {
				fmt.Fprintf(sb, " (class: %s, %d instance(s))", cell(f.Class), len(f.Instances))
			}
			sb.WriteString("\n")
			if f.Problem != "" {
				fmt.Fprintf(sb, "   %s\n", cell(f.Problem))
			}
		}
		sb.WriteString("\n</details>\n")
	}
}

// numberRuns orders runs by arm (the experiment's order, then unknown
// labels) and start, and names each "<arm> <n>".
func numberRuns(in *ReportInput) []armRun {
	order := map[string]int{}
	for i := range in.Experiment.Experiment.Arms {
		order[in.Experiment.Experiment.Arms[i].Name] = i
	}
	idx := make([]int, len(in.Runs))
	for i := range idx {
		idx[i] = i
	}
	rank := func(r *RunResult) int {
		if o, ok := order[r.Arm]; ok {
			return o
		}
		return len(order)
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ra, rb := &in.Runs[idx[a]], &in.Runs[idx[b]]
		if rank(ra) != rank(rb) {
			return rank(ra) < rank(rb)
		}
		if ra.Label != rb.Label {
			return ra.Label < rb.Label
		}
		return ra.StartedAt.Before(rb.StartedAt)
	})
	out := make([]armRun, 0, len(idx))
	count := map[string]int{}
	for _, i := range idx {
		r := &in.Runs[i]
		name := r.Arm
		if name == "" {
			name = r.Label
		}
		count[name]++
		ar := armRun{run: r, name: fmt.Sprintf("%s %d", name, count[name])}
		if in.Scores != nil {
			ar.score = in.Scores[r.ID]
		}
		out = append(out, ar)
	}
	return out
}

func groupRuns(runs []armRun) map[groupKey][]armRun {
	g := map[groupKey][]armRun{}
	for i := range runs {
		r := runs[i]
		arm := r.run.Arm
		if arm == "" {
			arm = r.run.Label
		}
		k := groupKey{arm, r.run.Group}
		g[k] = append(g[k], r)
	}
	return g
}

func sortedGroups(e *Experiment, groups map[groupKey][]armRun) []groupKey {
	order := map[string]int{}
	for i := range e.Arms {
		order[e.Arms[i].Name] = i
	}
	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, ok := order[keys[i].arm]
		if !ok {
			oi = len(order)
		}
		oj, ok := order[keys[j].arm]
		if !ok {
			oj = len(order)
		}
		if oi != oj {
			return oi < oj
		}
		if keys[i].arm != keys[j].arm {
			return keys[i].arm < keys[j].arm
		}
		return keys[i].group < keys[j].group
	})
	return keys
}

func missingRuns(e *Experiment, runs []armRun) []string {
	have := map[string]int{}
	for i := range runs {
		have[runs[i].run.Arm]++
	}
	var out []string
	for i := range e.Arms {
		a := &e.Arms[i]
		if n := have[a.Name]; n < e.Runs {
			out = append(out, fmt.Sprintf("%s has %d of %d", a.Name, n, e.Runs))
		}
	}
	return out
}

// refOf is arm's ref, with the commit prepare resolved it to when that was
// recorded.
func refOf(e *ExperimentRecord, arm string) string {
	for i := range e.Experiment.Arms {
		if a := &e.Experiment.Arms[i]; a.Name == arm {
			if c := e.Commits[a.Ref]; c != "" {
				return "`" + a.Ref + " @ " + short(c) + "`"
			}
			return "`" + a.Ref + "`"
		}
	}
	return dash
}

func findingAt(r *RunResult, n int) *Finding {
	if n < 1 || n > len(r.Findings) {
		return nil
	}
	return &r.Findings[n-1]
}

func defectMark(s *RunScore, id string) string {
	if s == nil {
		return " "
	}
	for i := range s.Defects {
		d := &s.Defects[i]
		if d.Defect != id {
			continue
		}
		m := map[string]string{Found: "✓", Partial: "½", Missed: "·"}[d.Effective()]
		if d.Override != "" {
			m += "*"
		}
		return m
	}
	return " "
}

func extraCounts(s *RunScore) (nReal, nFalse int) {
	for i := range s.Extras {
		switch s.Extras[i].Effective() {
		case Real:
			nReal++
		case False:
			nFalse++
		}
	}
	return nReal, nFalse
}

func recallText(s *RunScore) string {
	var sum float64
	for i := range s.Defects {
		sum += Credit(s.Defects[i].Effective())
	}
	return fmt.Sprintf("%s of %d", trimFloat(sum), len(s.Defects))
}

func trimFloat(f float64) string {
	if f == float64(int(f)) {
		return fmt.Sprint(int(f))
	}
	return fmt.Sprintf("%.1f", f)
}

func ended(r *RunResult) string {
	switch {
	case r.Error != "":
		return "error"
	case !r.HasResult:
		return "cut off"
	case r.Done:
		return "complete"
	case r.Reason != "":
		return cell(r.Reason)
	}
	return "incomplete"
}

func blocking(fs []Finding) int {
	n := 0
	for i := range fs {
		if fs[i].Blocking() {
			n++
		}
	}
	return n
}

func dur(d time.Duration) string {
	if d <= 0 {
		return dash
	}
	d = d.Round(time.Second)
	m := int(d / time.Minute)
	return fmt.Sprintf("%dm%02ds", m, int((d%time.Minute)/time.Second))
}

func perTurn(total float64, turns, decimals int) string {
	if turns == 0 {
		return dash
	}
	return fmt.Sprintf("%.*f", decimals, total/float64(turns))
}

func effortSuffix(e string) string {
	if e == "" {
		return ""
	}
	return " at " + e + " effort"
}

func shortHash(h string) string {
	_, rest, ok := strings.Cut(h, ":")
	if !ok {
		rest = h
	}
	if len(rest) > 8 {
		rest = rest[:8]
	}
	if rest == "" {
		return dash
	}
	return rest
}

// cell makes text safe in a table cell or a one-line list item.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

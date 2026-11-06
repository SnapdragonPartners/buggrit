package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"

	"github.com/SnapdragonPartners/buggrit/internal/bench"
	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/github/githubtest"
)

const (
	repoRoot  = "../.."
	fixture   = "bench/pr9-e619281.json"
	judgeJSON = `{"provider": "openai-compatible", "base_url": "https://gw.example/v1", "model": "judge-1", "api_key_env": "JUDGE_KEY"}`
)

// writeExperiment writes an experiment on the checked-in benchmark.
func writeExperiment(t *testing.T, extra string) string {
	t.Helper()
	spec := `{"name": "trial", "benchmark": "` + fixture + `", "judge": ` + judgeJSON + `, "runs": 2, "parallel": 1` + extra + `,
	  "arms": [{"name": "deepseek"}, {"name": "glm", "ref": "exp/glm", "overlay": {"models": {"find": {"model": "glm-5.3"}}, "limits": {"pass_deadline": "30m"}}}]}`
	path := filepath.Join(t.TempDir(), "experiment.json")
	if err := os.WriteFile(path, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestCheck(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), deps{}, []string{"check", "--root", repoRoot, writeExperiment(t, "")}, env(nil), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"2 arm(s) × 2 run(s), 1 at a time", "benchmark pr9-e619281: 10 known defects", "arm glm: ref exp/glm, label trial/glm, job timeout 45m", "arm deepseek: ref main, label trial/deepseek, job timeout 70m", "report timeout 22m"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if code := run(context.Background(), deps{}, []string{"check"}, env(nil), &out, &errb); code != 2 {
		t.Errorf("check without a file: exit %d", code)
	}
}

// fakeDeps points the GitHub client at fake and the judge at judge.
func fakeDeps(fake *githubtest.Server, db *memDB, judge llms.ChatClient) deps {
	return deps{
		github:    func(string, string) *github.Client { return fake.Client(githubtest.DefaultToken) },
		replayer:  func(gh *github.Client) bench.Replayer { return &bench.GitHubReplayer{GH: gh} },
		openStore: func(context.Context, string) (benchDB, error) { return db, nil },
		judge: func(m *config.Model, getenv func(string) string) (llms.ChatClient, error) {
			if m.Model != "judge-1" || getenv("JUDGE_KEY") != "k" {
				return nil, errors.New("wrong judge or key")
			}
			return judge, nil
		},
		newID: func() string { return "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" },
	}
}

func prepareEnv(output string) func(string) string {
	return env(map[string]string{"GITHUB_TOKEN": githubtest.DefaultToken, "GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REPOSITORY": "SnapdragonPartners/buggrit", "GITHUB_OUTPUT": output, "BUGGRIT_DATABASE_URL": "postgres://db"})
}

func TestPrepare(t *testing.T) {
	fake := githubtest.New(t, "SnapdragonPartners/buggrit")
	setRefs(fake, github.CompareAhead, github.CompareIdentical)
	output := filepath.Join(t.TempDir(), "output")
	var out, errb bytes.Buffer
	code := run(context.Background(), fakeDeps(fake, newMemDB(), nil), []string{"prepare", "--root", repoRoot, "--experiment", writeExperiment(t, "")}, prepareEnv(output), &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	outputs := readOutputs(t, output)
	var replay bench.Replay
	if err := json.Unmarshal([]byte(outputs["replay"]), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Repo != "SnapdragonPartners/buggrit" || replay.Head != "bench/trial/42-1/head" || replay.PR <= 0 {
		t.Fatalf("replay = %+v", replay)
	}
	if pr := fake.PR(replay.PR); !pr.Draft || pr.Title != "Verify earlier findings and drive each thread through its lifecycle" {
		t.Fatalf("PR = %+v", pr)
	}
	if outputs["parallel"] != "1" {
		t.Errorf("parallel = %q", outputs["parallel"])
	}
	// 4 runs to judge, 3 minutes each, plus 10.
	if outputs["report_timeout"] != "22" {
		t.Errorf("report_timeout = %q", outputs["report_timeout"])
	}
	var matrix struct{ Include []matrixEntry }
	if err := json.Unmarshal([]byte(outputs["matrix"]), &matrix); err != nil {
		t.Fatal(err)
	}
	if len(matrix.Include) != 4 {
		t.Fatalf("matrix = %+v", matrix)
	}
	// Each run carries its arm's ref and the commit it resolved to, which
	// the job checks out.
	for _, e := range matrix.Include {
		want := map[string]string{"deepseek": mainCommit, "glm": glmCommit}[e.Arm]
		if e.Commit != want {
			t.Errorf("%s run %d: commit %q, want %q", e.Arm, e.Run, e.Commit, want)
		}
	}
	var commits map[string]string
	if err := json.Unmarshal([]byte(outputs["commits"]), &commits); err != nil || len(commits) != 2 || commits["main"] != mainCommit || commits["exp/glm"] != glmCommit {
		t.Errorf("commits output = %s (%v)", outputs["commits"], err)
	}
	if !strings.Contains(errb.String(), "ref exp/glm @ "+glmCommit) {
		t.Errorf("prepare doesn't log the resolved refs:\n%s", errb.String())
	}
	glm := matrix.Include[3]
	if glm.Arm != "glm" || glm.Run != 2 || glm.Ref != "exp/glm" || glm.Label != "trial/glm" || glm.PR != replay.PR ||
		glm.Head != "e61928194c1995a6743b2eedde530c0480206313" || glm.Timeout != 45 {
		t.Fatalf("glm run 2 = %+v", glm)
	}
	cfg, err := config.Parse([]byte(glm.Config))
	if err != nil || cfg.Stage(config.StageFind).Model != "glm-5.3" || cfg.Stage(config.StageVerify).Model != "deepseek-4.1-flash" {
		t.Fatalf("glm config: %v\n%s", err, glm.Config)
	}
}

// The commits writeExperiment's arms' refs resolve to.
var (
	mainCommit = strings.Repeat("1", 40)
	glmCommit  = strings.Repeat("2", 40)
)

// commitsArg is prepare's commits output for writeExperiment's arms.
var commitsArg = `{"main": "` + mainCommit + `", "exp/glm": "` + glmCommit + `"}`

// setRefs resolves main and exp/glm, writeExperiment's refs, and sets how
// each commit compares to identityCommit.
func setRefs(fake *githubtest.Server, mainStatus, glmStatus string) {
	fake.SetCommit("main", mainCommit)
	fake.SetCommit("exp/glm", glmCommit)
	fake.SetCompare(identityCommit, mainCommit, githubtest.Comparison{Status: mainStatus})
	fake.SetCompare(identityCommit, glmCommit, githubtest.Comparison{Status: glmStatus})
}

func readOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		out[k] = v
	}
	return out
}

// Each distinct ref, whether a branch, a tag or a SHA, is resolved once to
// the commit every run of its arms builds, so a ref that moves mid-
// experiment doesn't split an arm across commits (#60). A ref GitHub
// doesn't know stops prepare before anything is opened.
func TestPrepareResolvesRefs(t *testing.T) {
	tagCommit, shaCommit := strings.Repeat("3", 40), strings.Repeat("4", 40)
	spec := `{"name": "refs", "benchmark": "` + fixture + `", "judge": ` + judgeJSON + `, "runs": 2,
	  "arms": [{"name": "branch"}, {"name": "branch-again", "overlay": {"limits": {"pass_deadline": "30m"}}},
	    {"name": "tag", "ref": "v0.3.0"}, {"name": "sha", "ref": "4444444"}]}`
	path := filepath.Join(t.TempDir(), "experiment.json")
	if err := os.WriteFile(path, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := githubtest.New(t, "SnapdragonPartners/buggrit")
	fake.SetCommit("main", mainCommit)
	fake.SetCommit("v0.3.0", tagCommit)
	fake.SetCommit("4444444", shaCommit)
	for _, c := range []string{mainCommit, tagCommit, shaCommit} {
		fake.SetCompare(identityCommit, c, githubtest.Comparison{Status: github.CompareAhead})
	}
	resolves := 0
	fake.OnRequest = func(op string) int {
		if op == "GET commit" {
			resolves++
		}
		return 0
	}
	output := filepath.Join(t.TempDir(), "output")
	var out, errb bytes.Buffer
	args := []string{"prepare", "--root", repoRoot, "--experiment", path}
	if code := run(context.Background(), fakeDeps(fake, newMemDB(), nil), args, prepareEnv(output), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if resolves != 3 {
		t.Errorf("resolved refs %d times, want once per distinct ref (3)", resolves)
	}
	var matrix struct{ Include []matrixEntry }
	if err := json.Unmarshal([]byte(readOutputs(t, output)["matrix"]), &matrix); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"branch": {"main", mainCommit}, "branch-again": {"main", mainCommit},
		"tag": {"v0.3.0", tagCommit}, "sha": {"4444444", shaCommit},
	}
	if len(matrix.Include) != 8 {
		t.Fatalf("matrix = %+v", matrix)
	}
	for _, e := range matrix.Include {
		if w := want[e.Arm]; e.Ref != w[0] || e.Commit != w[1] {
			t.Errorf("%s run %d: ref %q commit %q, want %q %q", e.Arm, e.Run, e.Ref, e.Commit, w[0], w[1])
		}
	}

	// A ref GitHub doesn't know.
	fake = githubtest.New(t, "SnapdragonPartners/buggrit")
	fake.SetCommit("main", mainCommit)
	fake.SetCompare(identityCommit, mainCommit, githubtest.Comparison{Status: github.CompareAhead})
	errb.Reset()
	code := run(context.Background(), fakeDeps(fake, newMemDB(), nil), []string{"prepare", "--root", repoRoot, "--experiment", writeExperiment(t, "")}, prepareEnv(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "arm glm: ref exp/glm: resolve exp/glm") || len(fake.Branches()) != 0 {
		t.Fatalf("unknown ref: exit %d, branches %v: %s", code, fake.Branches(), errb.String())
	}
}

// An arm whose ref predates the run identity is refused before anything
// is opened.
func TestPrepareRefusesAnOldRef(t *testing.T) {
	fake := githubtest.New(t, "SnapdragonPartners/buggrit")
	setRefs(fake, github.CompareAhead, github.CompareDiverged)
	var out, errb bytes.Buffer
	code := run(context.Background(), fakeDeps(fake, newMemDB(), nil), []string{"prepare", "--root", repoRoot, "--experiment", writeExperiment(t, "")}, prepareEnv(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "doesn't contain 216d2fa") || len(fake.Branches()) != 0 {
		t.Fatalf("exit %d, branches %v: %s", code, fake.Branches(), errb.String())
	}
}

// A database the runs couldn't record to stops the experiment before the
// replay is opened, so no model time is spent.
func TestPrepareChecksTheDatabase(t *testing.T) {
	fake := githubtest.New(t, "SnapdragonPartners/buggrit")
	setRefs(fake, github.CompareAhead, github.CompareAhead)
	db := newMemDB()
	db.writeErr = errors.New("this login can't write telemetry")
	var out, errb bytes.Buffer
	args := []string{"prepare", "--root", repoRoot, "--experiment", writeExperiment(t, "")}
	code := run(context.Background(), fakeDeps(fake, db, nil), args, prepareEnv(t.TempDir()+"/out"), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "the database: this login can't write telemetry") || len(fake.Branches()) != 0 {
		t.Fatalf("exit %d, branches %v: %s", code, fake.Branches(), errb.String())
	}
	noDB := func(k string) string {
		if k == "BUGGRIT_DATABASE_URL" {
			return ""
		}
		return prepareEnv("")(k)
	}
	if code = run(context.Background(), fakeDeps(fake, newMemDB(), nil), args, noDB, &out, &errb); code != 2 || len(fake.Branches()) != 0 {
		t.Fatalf("without a database: exit %d", code)
	}
	// The database action's reason is reported as it is, not as a usage
	// error.
	failed := func(k string) string {
		if k == "BUGGRIT_DATABASE_ERROR" {
			return "the identity provider refused the token"
		}
		return noDB(k)
	}
	errb.Reset()
	code = run(context.Background(), fakeDeps(fake, newMemDB(), nil), args, failed, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "the database couldn't be set up: the identity provider refused the token") || len(fake.Branches()) != 0 {
		t.Fatalf("a failed database action: exit %d: %s", code, errb.String())
	}
	// A workflow run that already recorded an experiment would get this
	// one's scores filed under it: refused, before the replay opens. Another
	// attempt of it is fine: a re-run over an experiment its first attempt
	// recorded.
	db = newMemDB()
	db.experiment = &bench.ExperimentRecord{ID: "e1", Experiment: &bench.Experiment{Name: "earlier"}, Workflow: bench.WorkflowRun{ID: 42, Attempt: 1}}
	errb.Reset()
	code = run(context.Background(), fakeDeps(fake, db, nil), args, prepareEnv(t.TempDir()+"/out"), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "workflow run 42 attempt 1 already recorded experiment earlier") || len(fake.Branches()) != 0 {
		t.Fatalf("a used workflow run: exit %d, branches %v: %s", code, fake.Branches(), errb.String())
	}
	if code = run(context.Background(), fakeDeps(fake, db, nil), append(args, "--attempt", "2"), prepareEnv(t.TempDir()+"/out"), &out, &errb); code != 0 {
		t.Fatalf("another attempt: exit %d: %s", code, errb.String())
	}
	// A database that can't say whether the run is used refuses too.
	fake = githubtest.New(t, "SnapdragonPartners/buggrit")
	setRefs(fake, github.CompareAhead, github.CompareAhead)
	db = newMemDB()
	db.loadErr = errors.New("connection reset")
	errb.Reset()
	code = run(context.Background(), fakeDeps(fake, db, nil), args, prepareEnv(t.TempDir()+"/out"), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "the database: connection reset") || len(fake.Branches()) != 0 {
		t.Fatalf("a failed read: exit %d, branches %v: %s", code, fake.Branches(), errb.String())
	}
}

// If the outputs can't be written, nothing downstream knows the replay, so
// prepare closes it.
func TestPrepareClosesTheReplayOnFailure(t *testing.T) {
	fake := githubtest.New(t, "SnapdragonPartners/buggrit")
	setRefs(fake, github.CompareAhead, github.CompareAhead)
	var out, errb bytes.Buffer
	code := run(context.Background(), fakeDeps(fake, newMemDB(), nil), []string{"prepare", "--root", repoRoot, "--experiment", writeExperiment(t, "")}, prepareEnv(t.TempDir()), &out, &errb)
	if code != 1 || len(fake.Branches()) != 0 || !fake.PR(1).Closed {
		t.Fatalf("exit %d, branches %v: %s", code, fake.Branches(), errb.String())
	}
}

func TestCleanup(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	replay, err := (&bench.GitHubReplayer{GH: fake.Client(githubtest.DefaultToken)}).Open(context.Background(),
		&bench.Target{Repo: "o/r", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), Title: "T"}, "trial/1-1")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(replay)
	var out, errb bytes.Buffer
	if code := run(context.Background(), fakeDeps(fake, nil, nil), []string{"cleanup", "--replay", string(data)}, prepareEnv(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !fake.PR(replay.PR).Closed || len(fake.Branches()) != 0 {
		t.Fatal("not cleaned up")
	}
	// A replay that isn't one of prepare's is refused.
	if code := run(context.Background(), fakeDeps(fake, nil, nil), []string{"cleanup", "--replay", `{"repo": "o/r", "pr": 1, "head": "main", "base": "dev"}`}, prepareEnv(""), &out, &errb); code != 2 {
		t.Fatalf("cleanup of a non-replay: exit %d", code)
	}
}

// memDB is an in-memory bench.Store.
type memDB struct {
	scores     map[string]*bench.RunScore
	experiment *bench.ExperimentRecord
	benchmarks map[string]*bench.Benchmark
	runs       []bench.RunResult
	writeErr   error
	loadErr    error
	mu         sync.Mutex
}

func (m *memDB) Close() error { return nil }

func (m *memDB) CheckWrites(context.Context) error { return m.writeErr }

func (m *memDB) SaveBenchmark(_ context.Context, hash string, b *bench.Benchmark) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.benchmarks[hash] = b
	return nil
}

func (m *memDB) SaveExperiment(_ context.Context, e *bench.ExperimentRecord) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.experiment == nil {
		m.experiment = e
	}
	return m.experiment.ID, nil
}

func (m *memDB) LoadExperiment(_ context.Context, w bench.WorkflowRun) (*bench.ExperimentRecord, error) {
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	if m.experiment == nil || m.experiment.Workflow != w {
		return nil, bench.ErrNoExperiment
	}
	return m.experiment, nil
}

func (m *memDB) Runs(_ context.Context, w bench.WorkflowRun, r *bench.Replay) ([]bench.RunResult, error) {
	if w != (bench.WorkflowRun{ID: 42, Attempt: 1}) || r.PR != 11 {
		return nil, errors.New("wrong workflow run or replay")
	}
	return append([]bench.RunResult(nil), m.runs...), nil
}

func (m *memDB) SaveScore(_ context.Context, _ string, s *bench.RunScore, _ []bench.Finding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.scores[s.RunID] == nil {
		m.scores[s.RunID] = s
	}
	return nil
}

func (m *memDB) Scores(context.Context, string) (map[string]*bench.RunScore, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]*bench.RunScore{}
	for k, v := range m.scores {
		c := *v
		out[k] = &c
	}
	return out, nil
}

func newMemDB() *memDB {
	return &memDB{
		scores: map[string]*bench.RunScore{}, benchmarks: map[string]*bench.Benchmark{},
		runs: []bench.RunResult{
			{ID: "r1", Label: "trial/deepseek", Group: "g1", HasResult: true, Done: true, ElapsedMS: 60000, Turns: 3,
				Findings: []bench.Finding{{Path: "CLAUDE.md", Line: 35, Severity: "P2", Title: "stale layout"}}},
			{ID: "r2", Label: "trial/glm", Group: "g2", HasResult: true, Done: true, ElapsedMS: 30000, Turns: 2},
			{ID: "r3", Label: "trial/deepseek", Group: "g1"}, // cut off before its result
		},
	}
}

// judgment marks stale-layout-docs found and misses the rest.
func judgment(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, fixture))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := bench.ParseBenchmark(data)
	if err != nil {
		t.Fatal(err)
	}
	var defects []map[string]any
	for _, d := range b.Defects {
		v, f := bench.Missed, 0
		if d.ID == "stale-layout-docs" {
			v, f = bench.Found, 1
		}
		defects = append(defects, map[string]any{"id": d.ID, "verdict": v, "finding": f, "reasoning": "r"})
	}
	out, _ := json.Marshal(map[string]any{"defects": defects, "extras": []any{}})
	return string(out)
}

func reportEnv(summary string) func(string) string {
	return env(map[string]string{
		"GITHUB_TOKEN": githubtest.DefaultToken, "GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "1",
		"GITHUB_REPOSITORY": "o/r", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_STEP_SUMMARY": summary,
		"BUGGRIT_DATABASE_URL": "postgres://db", "JUDGE_KEY": "k",
	})
}

func TestReport(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	db := newMemDB()
	judge := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("c", "submit_judgment", judgment(t))}}
	replay := `{"repo": "o/r", "pr": 11, "base": "bench/trial/42-1/base", "head": "bench/trial/42-1/head"}`
	dir := t.TempDir()
	summary, out := filepath.Join(dir, "summary.md"), filepath.Join(dir, "report.md")
	var stdout, stderr bytes.Buffer
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay, "--commits", commitsArg, "--out", out}
	if code := run(context.Background(), fakeDeps(fake, db, judge), args, reportEnv(summary), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	// r2 has no findings, so it's scored without a call; r3 has no result.
	if n := len(judge.Calls()); n != 1 {
		t.Errorf("%d judge calls, want 1", n)
	}
	if len(db.scores) != 2 || db.scores["r1"].Recall() != 0.1 || db.scores["r3"] != nil {
		t.Fatalf("scores = %+v", db.scores)
	}
	report, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Benchmark: trial", "**Missing runs:** glm has 1 of 2", "| deepseek 1 |", "| 1 of 10 |", "| deepseek 2 |", "cut off", "`main @ 1111111`", "not scored", "[Workflow run](https://github.com/o/r/actions/runs/42/attempts/1)"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if c := db.experiment.Commits; c["main"] != mainCommit || c["exp/glm"] != glmCommit {
		t.Errorf("recorded commits = %v", c)
	}
	if s, _ := os.ReadFile(summary); string(s) != string(report) {
		t.Error("the job summary isn't the report")
	}

	// Regenerating reads the experiment back and judges nothing again.
	stdout.Reset()
	if code := run(context.Background(), fakeDeps(fake, db, judge), []string{"report", "--no-judge"}, reportEnv(""), &stdout, &stderr); code != 0 {
		t.Fatalf("regenerate: exit %d: %s", code, stderr.String())
	}
	if len(judge.Calls()) != 1 || !strings.Contains(stdout.String(), "| 1 of 10 |") {
		t.Fatalf("regenerated report:\n%s", stdout.String())
	}
}

// A workflow run that already recorded another experiment (another
// replay) is refused before anything is scored under it; the same
// experiment's report again goes ahead.
func TestReportRefusesAUsedWorkflowRun(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	judge := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("c", "submit_judgment", judgment(t))}}
	replay := `{"repo": "o/r", "pr": 11, "base": "bench/trial/42-1/base", "head": "bench/trial/42-1/head"}`
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay, "--commits", commitsArg}
	for name, tc := range map[string]struct {
		pr   int
		code int
	}{"another experiment": {pr: 9, code: 1}, "the same one again": {pr: 11}} {
		t.Run(name, func(t *testing.T) {
			db := newMemDB()
			db.experiment = &bench.ExperimentRecord{
				ID: "e1", Experiment: &bench.Experiment{Name: "earlier"}, Workflow: bench.WorkflowRun{ID: 42, Attempt: 1},
				Replay: &bench.Replay{Repo: "o/r", PR: tc.pr},
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), fakeDeps(fake, db, judge), args, reportEnv(""), &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, stderr.String())
			}
			refused := strings.Contains(stderr.String(), "workflow run 42 attempt 1 already recorded experiment earlier")
			if refused != (tc.code != 0) || (tc.code != 0 && len(db.scores) != 0) {
				t.Fatalf("refused %v, scores %v: %s", refused, db.scores, stderr.String())
			}
		})
	}
}

// A run the judge can't score is left unscored and the report still goes
// out, but the step fails.
func TestReportJudgeFailure(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	db := newMemDB()
	judge := &testllm.FakeChatClient{Err: errors.New("judge down")}
	replay := `{"repo": "o/r", "pr": 11, "base": "bench/t/base", "head": "bench/t/head"}`
	var stdout, stderr bytes.Buffer
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay, "--commits", commitsArg}
	if code := run(context.Background(), fakeDeps(fake, db, judge), args, reportEnv(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d", code)
	}
	// r2 has no findings, so it needs no judge and is scored.
	if !strings.Contains(stderr.String(), "judge down") || db.scores["r1"] != nil || db.scores["r2"] == nil || !strings.Contains(stdout.String(), "not scored") {
		t.Fatalf("stderr %s\nscores %+v\n%s", stderr.String(), db.scores, stdout.String())
	}
}

// Without the judge's key, a run with no findings is still scored: it
// needs no call.
func TestReportWithoutJudgeKey(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	db := newMemDB()
	replay := `{"repo": "o/r", "pr": 11, "base": "bench/t/base", "head": "bench/t/head"}`
	noKey := func(k string) string {
		if k == "JUDGE_KEY" {
			return ""
		}
		return reportEnv("")(k)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay, "--commits", commitsArg}
	if code := run(context.Background(), fakeDeps(fake, db, nil), args, noKey, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "wrong judge or key") || !strings.Contains(stderr.String(), "1 run(s) left unscored") || db.scores["r1"] != nil || db.scores["r2"] == nil {
		t.Fatalf("stderr %s\nscores %+v", stderr.String(), db.scores)
	}
}

// A run whose label names no arm is still judged, with a warning, so it
// can't be dropped silently.
func TestReportJudgesAnUnattributedRun(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	db := newMemDB()
	db.runs = append(db.runs, bench.RunResult{ID: "r4", Label: "renamed/deepseek", HasResult: true, Done: true})
	judge := &testllm.FakeChatClient{Responses: []llms.ChatResponse{testllm.ToolCallResponse("c", "submit_judgment", judgment(t))}}
	replay := `{"repo": "o/r", "pr": 11, "base": "bench/t/base", "head": "bench/t/head"}`
	var stdout, stderr bytes.Buffer
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay, "--commits", commitsArg}
	if code := run(context.Background(), fakeDeps(fake, db, judge), args, reportEnv(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if db.scores["r4"] == nil || !strings.Contains(stderr.String(), `run r4's label "renamed/deepseek" names none of the experiment's arms`) {
		t.Fatalf("stderr %s\nscores %+v", stderr.String(), db.scores)
	}
}

func TestReportUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cases := [][]string{
		{"report", "--experiment", "x.json"},
		{"report", "--workflow-run", "0"},
		// Each command takes only the flags it uses: report doesn't call
		// GitHub, and cleanup reads nothing of the workflow run.
		{"report", "--api-url", "https://api.example"},
		{"cleanup", "--workflow-run", "42", "--replay", "{}"},
		// prepare's three outputs go together.
		{"report", "--experiment", "x.json", "--replay", "{}"},
		{"report", "--commits", "{}"},
	}
	for _, args := range cases {
		if code := run(context.Background(), deps{}, args, reportEnv(""), &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	noDB := env(map[string]string{"GITHUB_RUN_ID": "42"})
	if code := run(context.Background(), deps{}, []string{"report"}, noDB, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "BUGGRIT_DATABASE_URL") {
		t.Errorf("no database: exit %d: %s", code, stderr.String())
	}
	dbErr := env(map[string]string{"GITHUB_RUN_ID": "42", "BUGGRIT_DATABASE_ERROR": "proxy didn't start"})
	if code := run(context.Background(), deps{}, []string{"report"}, dbErr, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "proxy didn't start") {
		t.Errorf("database error: exit %d: %s", code, stderr.String())
	}
	// Commits that miss an arm's ref aren't prepare's.
	db := newMemDB()
	replay := `{"repo": "o/r", "pr": 11, "base": "bench/t/base", "head": "bench/t/head"}`
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay, "--commits", `{"main": "` + mainCommit + `"}`}
	stderr.Reset()
	if code := run(context.Background(), fakeDeps(githubtest.New(t, "o/r"), db, nil), args, reportEnv(""), &stdout, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "no commit for arm glm's ref exp/glm") || db.experiment != nil {
		t.Errorf("incomplete commits: exit %d: %s", code, stderr.String())
	}
}

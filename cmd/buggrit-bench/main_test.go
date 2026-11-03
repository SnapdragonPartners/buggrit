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
	fake.SetCompare(identityCommit, "main", githubtest.Comparison{Status: github.CompareAhead})
	fake.SetCompare(identityCommit, "exp/glm", githubtest.Comparison{Status: github.CompareIdentical})
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

// An arm whose ref predates the run identity is refused before anything
// is opened.
func TestPrepareRefusesAnOldRef(t *testing.T) {
	fake := githubtest.New(t, "SnapdragonPartners/buggrit")
	fake.SetCompare(identityCommit, "main", githubtest.Comparison{Status: github.CompareAhead})
	fake.SetCompare(identityCommit, "exp/glm", githubtest.Comparison{Status: github.CompareDiverged})
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
	fake.SetCompare(identityCommit, "main", githubtest.Comparison{Status: github.CompareAhead})
	fake.SetCompare(identityCommit, "exp/glm", githubtest.Comparison{Status: github.CompareAhead})
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
}

// If the outputs can't be written, nothing downstream knows the replay, so
// prepare closes it.
func TestPrepareClosesTheReplayOnFailure(t *testing.T) {
	fake := githubtest.New(t, "SnapdragonPartners/buggrit")
	fake.SetCompare(identityCommit, "main", githubtest.Comparison{Status: github.CompareAhead})
	fake.SetCompare(identityCommit, "exp/glm", githubtest.Comparison{Status: github.CompareAhead})
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
	if m.experiment == nil || m.experiment.Workflow != w {
		return nil, errors.New("no experiment")
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
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay, "--out", out}
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
	for _, want := range []string{"## Benchmark: trial", "**Missing runs:** glm has 1 of 2", "| deepseek 1 |", "| 1 of 10 |", "| deepseek 2 |", "cut off", "not scored", "[Workflow run](https://github.com/o/r/actions/runs/42/attempts/1)"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
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

// A run the judge can't score is left unscored and the report still goes
// out, but the step fails.
func TestReportJudgeFailure(t *testing.T) {
	fake := githubtest.New(t, "o/r")
	db := newMemDB()
	judge := &testllm.FakeChatClient{Err: errors.New("judge down")}
	replay := `{"repo": "o/r", "pr": 11, "base": "bench/t/base", "head": "bench/t/head"}`
	var stdout, stderr bytes.Buffer
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay}
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
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay}
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
	args := []string{"report", "--root", repoRoot, "--experiment", writeExperiment(t, ""), "--replay", replay}
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
}

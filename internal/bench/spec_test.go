package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

const (
	shaA = "6d36cd0bc99ee5b1db0239fec3ad6649767010f9"
	shaB = "e61928194c1995a6743b2eedde530c0480206313"
)

// The checked-in benchmarks parse.
func TestCheckedInBenchmarks(t *testing.T) {
	entries, err := os.ReadDir("../../bench")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
		data, err := os.ReadFile("../../bench/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		b, hash, err := ParseBenchmark(data)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if b.Name+".json" != e.Name() {
			t.Errorf("%s: name %q doesn't match the file", e.Name(), b.Name)
		}
		if !strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+64 {
			t.Errorf("%s: hash %q", e.Name(), hash)
		}
	}
	if n == 0 {
		t.Fatal("no benchmarks in bench/")
	}
}

func benchmarkJSON(mut func(m map[string]any)) []byte {
	m := map[string]any{
		"name": "b1", "repo": "o/r", "base": shaA, "head": shaB, "title": "T",
		"defects": []any{
			map[string]any{"id": "d1", "source": "copilot", "description": "one", "locations": []any{map[string]any{"path": "a.go", "start": 1, "end": 3}}},
			map[string]any{"id": "d2", "source": "buggrit", "description": "two", "locations": []any{map[string]any{"path": "b.md"}}},
		},
	}
	if mut != nil {
		mut(m)
	}
	data, _ := json.Marshal(m)
	return data
}

// defect is benchmarkJSON's defect i, to change.
func defect(m map[string]any, i int) map[string]any {
	ds, _ := m["defects"].([]any)
	d, _ := ds[i].(map[string]any)
	return d
}

func TestParseBenchmark(t *testing.T) {
	b, hash, err := ParseBenchmark(benchmarkJSON(nil))
	if err != nil || len(b.Defects) != 2 || b.Repo != "o/r" || hash == "" {
		t.Fatalf("ParseBenchmark = %+v, %q, %v", b, hash, err)
	}
	// The hash is of the bytes: any edit makes a new benchmark.
	_, hash2, _ := ParseBenchmark(append(benchmarkJSON(nil), '\n'))
	if hash2 == hash {
		t.Error("a different file has the same hash")
	}

	bad := []struct {
		name string
		mut  func(m map[string]any)
		want string
	}{
		{"short sha", func(m map[string]any) { m["head"] = "e619281" }, "head must be a full commit SHA"},
		{"bad repo", func(m map[string]any) { m["repo"] = "nope" }, "target.repo"},
		{"no title", func(m map[string]any) { m["title"] = " " }, "title is required"},
		{"no defects", func(m map[string]any) { m["defects"] = []any{} }, "at least one"},
		{"unknown key", func(m map[string]any) { m["extra"] = 1 }, "unknown field"},
		{"bad name", func(m map[string]any) { m["name"] = "Has Space" }, "name must be"},
		{"double dot", func(m map[string]any) { m["name"] = "a..b" }, "name must be"},
		{"trailing dot", func(m map[string]any) { m["name"] = "a." }, "name must be"},
		{"lock suffix", func(m map[string]any) { m["name"] = "a.lock" }, "name must be"},
		{"repeated defect", func(m map[string]any) { defect(m, 1)["id"] = "d1" }, `"d1" is repeated`},
		{"half range", func(m map[string]any) {
			defect(m, 0)["locations"] = []any{map[string]any{"path": "a.go", "start": 3}}
		}, "both set or both 0"},
		{"no locations", func(m map[string]any) { defect(m, 0)["locations"] = []any{} }, "locations: at least one"},
	}
	for _, c := range bad {
		if _, _, err := ParseBenchmark(benchmarkJSON(c.mut)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

const judgeModel = `{"provider": "openai-compatible", "base_url": "https://gw.example/v1", "model": "judge-1", "api_key_env": "K"}`

// Names follow git's rules for a ref component, since an experiment's is
// in its replay's branches.
func TestValidName(t *testing.T) {
	for _, s := range []string{"a", "a-1", "v1.0", "deepseek_4.1"} {
		if !validName(s) {
			t.Errorf("validName(%q) = false", s)
		}
	}
	for _, s := range []string{"", "a..b", "a.", "a.lock", ".a", "_a", "-a", "A", "a b", "a/b"} {
		if validName(s) {
			t.Errorf("validName(%q) = true", s)
		}
	}
}

func TestParseExperimentDefaults(t *testing.T) {
	e, err := ParseExperiment([]byte(`{"name": "x", "benchmark": "bench/b.json", "judge": ` + judgeModel + `, "arms": [{"name": "a"}, {"name": "b", "ref": "exp/tight-prompt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Runs != DefaultRuns || e.Parallel != DefaultParallel || e.BaseConfig != config.Path || e.Arms[0].Ref != "main" || e.Arms[1].Ref != "exp/tight-prompt" {
		t.Fatalf("defaults: %+v", e)
	}
	if got := e.Label("b"); got != "x/b" {
		t.Errorf("Label = %q", got)
	}
}

func TestParseExperimentErrors(t *testing.T) {
	target := `"target": {"repo": "o/r", "base": "` + shaA + `", "head": "` + shaB + `", "title": "T"}`
	cases := []struct{ name, spec, want string }{
		{"no judge", `{"name": "x", "benchmark": "b.json", "arms": [{"name": "a"}]}`, "needs a judge; there's no default"},
		{"judge without benchmark", `{"name": "x", ` + target + `, "judge": ` + judgeModel + `, "arms": [{"name": "a"}]}`, "only a benchmark is judged"},
		{"both", `{"name": "x", "benchmark": "b.json", ` + target + `, "judge": ` + judgeModel + `, "arms": [{"name": "a"}]}`, "not both"},
		{"neither", `{"name": "x", "arms": [{"name": "a"}]}`, "give a benchmark or a target"},
		{"bad judge", `{"name": "x", "benchmark": "b.json", "judge": {"provider": "nope", "model": "m"}, "arms": [{"name": "a"}]}`, "judge:"},
		{"no arms", `{"name": "x", ` + target + `, "arms": []}`, "arms: at least one"},
		{"repeated arm", `{"name": "x", ` + target + `, "arms": [{"name": "a"}, {"name": "a"}]}`, `"a" is repeated`},
		{"name not a ref", `{"name": "x.lock", ` + target + `, "arms": [{"name": "a"}]}`, "name must be"},
		{"arm not a ref", `{"name": "x", ` + target + `, "arms": [{"name": "a..b"}]}`, "arms[0].name must be"},
		{"bad ref", `{"name": "x", ` + target + `, "arms": [{"name": "a", "ref": "--upload-pack=x"}]}`, "is not a ref"},
		{"too many runs", `{"name": "x", ` + target + `, "runs": 99, "arms": [{"name": "a"}]}`, "runs must be"},
		{"unknown key", `{"name": "x", ` + target + `, "arms": [{"name": "a"}], "model": "m"}`, "unknown field"},
		{"too many jobs", `{"name": "x", ` + target + `, "runs": 20, "arms": [` + manyArms(13) + `]}`, "260 review jobs; a workflow's matrix allows 256"},
		{"trailing", `{"name": "x", ` + target + `, "arms": [{"name": "a"}]} {}`, "trailing data"},
	}
	for _, c := range cases {
		if _, err := ParseExperiment([]byte(c.spec)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := ParseExperiment([]byte(`{"name": "x", ` + target + `, "arms": [{"name": "a"}]}`)); err != nil {
		t.Errorf("a target without a judge: %v", err)
	}
}

// manyArms is n arms, a0 to a(n-1), as JSON array elements.
func manyArms(n int) string {
	arms := make([]string, n)
	for i := range arms {
		arms[i] = fmt.Sprintf(`{"name": "a%d"}`, i)
	}
	return strings.Join(arms, ", ")
}

const baseConfig = `{
  "version": 1,
  "models": {"default": {"provider": "openai-compatible", "base_url": "https://gw.example/v1", "model": "deepseek", "api_key_env": "K", "reasoning_effort": "medium"}},
  "limits": {"pass_deadline": "55m", "max_concurrency": 2},
  "review": {"instructions": "be brief"}
}`

func TestMerge(t *testing.T) {
	out, err := Merge([]byte(baseConfig), []byte(`{"models": {"default": {"model": "glm"}, "find": {"reasoning_effort": "low"}}, "limits": {"pass_deadline": "30m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	find := cfg.Stage(config.StageFind)
	if find.Model != "glm" || find.ReasoningEffort != "low" || find.BaseURL != "https://gw.example/v1" {
		t.Errorf("find model = %+v", find)
	}
	if cfg.Stage(config.StageVerify).ReasoningEffort != "medium" {
		t.Error("the overlay's find override leaked into verify")
	}
	if cfg.Limits.PassDeadline.Std().Minutes() != 30 || cfg.Limits.MaxConcurrency != 2 || cfg.Review.Instructions != "be brief" {
		t.Errorf("limits and review: %+v %+v", cfg.Limits, cfg.Review)
	}

	// No overlay is the base config, validated.
	if _, err := Merge([]byte(baseConfig), nil); err != nil {
		t.Errorf("no overlay: %v", err)
	}

	bad := []struct{ name, base, overlay, want string }{
		{"review", baseConfig, `{"review": {"min_severity": "P0"}}`, "review can't be overlaid"},
		{"telemetry", baseConfig, `{"telemetry": {"enabled": false}}`, "telemetry can't be overlaid"},
		{"other limit", baseConfig, `{"limits": {"max_tool_iterations": 3}}`, "limits.max_tool_iterations can't be overlaid"},
		{"not a stage", baseConfig, `{"models": {"judge": {"model": "x"}}}`, "models.judge is not a stage"},
		{"unknown model field", baseConfig, `{"models": {"find": {"temperature": 1}}}`, "models.find.temperature can't be overlaid"},
		{"invalid result", baseConfig, `{"models": {"find": {"reasoning_effort": "extreme"}}}`, "merged config"},
		{"null base", "null", `{"models": {"find": {"model": "x"}}}`, "must be a JSON object"},
		{"trailing data in base", baseConfig + ` {"version": 2}`, `{}`, "trailing data"},
		{"trailing data in overlay", baseConfig, `{} x`, "trailing data"},
		{"telemetry off in base", strings.Replace(baseConfig, `"version": 1,`, `"version": 1, "telemetry": {"enabled": false},`, 1), `{}`, "telemetry is off"},
	}
	for _, c := range bad {
		if _, err := Merge([]byte(c.base), []byte(c.overlay)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestJudgeConfigDefaults(t *testing.T) {
	var m config.Model
	if err := json.Unmarshal([]byte(judgeModel), &m); err != nil {
		t.Fatal(err)
	}
	cfg, err := JudgeConfig(&m)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Stage(config.StageFind); got.Model != "judge-1" || got.RequestTimeout <= 0 {
		t.Errorf("judge model = %+v", got)
	}
}

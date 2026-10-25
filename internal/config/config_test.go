package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimal = `{
  "version": 1,
  "models": {"default": {"provider": "openai-compatible", "base_url": "https://llm.example", "model": "qwen3-coder", "api_key_env": "BUGGRIT_LLM_API_KEY"}}
}`

func TestParseMinimalAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	def := Defaults()
	if cfg.Limits != def.Limits {
		t.Errorf("limits = %+v, want defaults %+v", cfg.Limits, def.Limits)
	}
	if cfg.Review.MinSeverity != SeverityP3 || cfg.Review.MinConfidence != ConfidenceMedium {
		t.Errorf("review thresholds = %s/%s, want P3/medium", cfg.Review.MinSeverity, cfg.Review.MinConfidence)
	}
	if !slices.Equal(cfg.Review.Exclude, def.Review.Exclude) {
		t.Errorf("exclude = %v, want %v", cfg.Review.Exclude, def.Review.Exclude)
	}
	if cfg.Convergence != def.Convergence || cfg.Gate != def.Gate || cfg.Identity != def.Identity {
		t.Errorf("convergence/gate/identity not defaulted: %+v %+v %+v", cfg.Convergence, cfg.Gate, cfg.Identity)
	}
	if got := cfg.Models.Default.RequestTimeout.Std(); got != 10*time.Minute {
		t.Errorf("request_timeout = %v, want 10m", got)
	}
}

// convergence.max_rebuttals is no longer used. A version 1 config may still
// set it: it's accepted, and warned about.
func TestMaxRebuttalsWarns(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil || len(cfg.Warnings()) != 0 {
		t.Fatalf("minimal config: %v, warnings %v", err, cfg.Warnings())
	}
	withRebuttals := strings.Replace(minimal, `"version": 1,`, `"version": 1, "convergence": {"max_rebuttals": 2},`, 1)
	cfg, err = Parse([]byte(withRebuttals))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if w := cfg.Warnings(); len(w) != 1 || !strings.Contains(w[0], "max_rebuttals is ignored") {
		t.Errorf("warnings = %v", w)
	}
}

func TestParseOverridesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`{
	  "version": 1,
	  "models": {"default": {"provider": "ollama", "model": "qwen3-coder:30b", "request_timeout": "90s"}},
	  "limits": {"profile": "budget", "pass_deadline": "20m", "max_tokens_per_pass": 500000},
	  "review": {"exclude": [], "min_severity": "P1"},
	  "gate": {"fail_on": "P1"}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Limits.Profile != ProfileBudget || cfg.Limits.PassDeadline.Std() != 20*time.Minute {
		t.Errorf("limits = %+v", cfg.Limits)
	}
	if cfg.Limits.MaxConcurrency != 4 {
		t.Errorf("unset max_concurrency = %d, want default 4", cfg.Limits.MaxConcurrency)
	}
	if cfg.Review.Exclude == nil || len(cfg.Review.Exclude) != 0 {
		t.Errorf("explicit empty exclude = %#v, want empty (not the default list)", cfg.Review.Exclude)
	}
	if !slices.Equal(cfg.Review.Include, []string{"**"}) {
		t.Errorf("unset include = %v, want default", cfg.Review.Include)
	}
	if cfg.Models.Default.RequestTimeout.Std() != 90*time.Second {
		t.Errorf("request_timeout = %v", cfg.Models.Default.RequestTimeout.Std())
	}
}

func TestStageOverrideMergesFieldByField(t *testing.T) {
	cfg, err := Parse([]byte(`{
	  "version": 1,
	  "models": {
	    "default": {"provider": "openai-compatible", "base_url": "https://llm.example", "model": "qwen3-coder", "api_key_env": "KEY"},
	    "verify": {"model": "glm-4.6", "reasoning_effort": "low"}
	  }
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := cfg.Stage(StageVerify)
	if v.Model != "glm-4.6" || v.ReasoningEffort != "low" {
		t.Errorf("verify model = %q at %q, want glm-4.6 at low", v.Model, v.ReasoningEffort)
	}
	if v.Provider != ProviderOpenAICompatible || v.BaseURL != "https://llm.example" || v.APIKeyEnv != "KEY" {
		t.Errorf("verify did not inherit unset fields: %+v", v)
	}
	if f := cfg.Stage(StageFind); f != cfg.Models.Default {
		t.Errorf("find without override = %+v, want default %+v", f, cfg.Models.Default)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name, doc, want string
	}{
		{"unknown key", `{"version": 1, "modles": {}}`, `unknown field "modles"`},
		{"unknown nested key", strings.Replace(minimal, `"model": "qwen3-coder"`, `"model": "qwen3-coder", "temp": 0`, 1), `unknown field "temp"`},
		{"trailing data", minimal + `{}`, "unexpected data"},
		{"trailing close bracket", minimal + `] not-json`, "unexpected data"},
		{"trailing close brace", minimal + `}`, "unexpected data"},
		{"negative iterations", strings.Replace(minimal, `"version": 1,`, `"version": 1, "limits": {"max_tool_iterations": -1},`, 1), "max_tool_iterations"},
		{"explicit empty stage provider", strings.Replace(minimal, `}}`, `}, "verify": {"provider": ""}}`, 1), "models.verify: provider is required"},
		{"explicit empty stage model", strings.Replace(minimal, `}}`, `}, "verify": {"model": ""}}`, 1), "models.verify: model is required"},
		{"explicit zero stage timeout", strings.Replace(minimal, `}}`, `}, "verify": {"request_timeout": "0s"}}`, 1), "models.verify: request_timeout must be positive"},
		{"missing version", `{"models": {"default": {"provider": "ollama", "model": "m"}}}`, "version is required"},
		{"future version", strings.Replace(minimal, `"version": 1`, `"version": 2`, 1), "version 2 is not supported"},
		{"missing model", `{"version": 1}`, "models.default: model is required"},
		{"unknown provider", `{"version": 1, "models": {"default": {"provider": "lunaroute", "model": "m"}}}`, `unknown provider "lunaroute"`},
		{"hosted provider without key", `{"version": 1, "models": {"default": {"provider": "anthropic", "model": "m"}}}`, "needs api_key_env"},
		{"compatible without base_url", `{"version": 1, "models": {"default": {"provider": "openai-compatible", "model": "m"}}}`, "needs base_url"},
		{"bad env name", strings.Replace(minimal, "BUGGRIT_LLM_API_KEY", "MY-KEY", 1), "not a valid environment variable name"},
		{"bad base_url", strings.Replace(minimal, "https://llm.example", "llm.example", 1), "must be an http:// or https:// URL with a host"},
		{"base_url without host", strings.Replace(minimal, "https://llm.example", "https://", 1), "URL with a host"},
		{"base_url with empty host and path", strings.Replace(minimal, "https://llm.example", "https:///v1", 1), "URL with a host"},
		{"base_url other scheme", strings.Replace(minimal, "https://llm.example", "ftp://llm.example", 1), "URL with a host"},
		{"ollama with api key", `{"version": 1, "models": {"default": {"provider": "ollama", "model": "m", "api_key_env": "K"}}}`, "does not support api_key_env"},
		{"numeric duration", strings.Replace(minimal, `"version": 1,`, `"version": 1, "limits": {"pass_deadline": 1800},`, 1), "duration must be a string"},
		{"bad duration", strings.Replace(minimal, `"version": 1,`, `"version": 1, "limits": {"pass_deadline": "soon"},`, 1), `invalid duration "soon"`},
		{"bad severity", strings.Replace(minimal, `"version": 1,`, `"version": 1, "review": {"min_severity": "P4"},`, 1), "min_severity"},
		{"bad gate", strings.Replace(minimal, `"version": 1,`, `"version": 1, "gate": {"fail_on": "P2"}`+",", 1), "gate.fail_on"},
		{"zero concurrency", strings.Replace(minimal, `"version": 1,`, `"version": 1, "limits": {"max_concurrency": 0},`, 1), "max_concurrency"},
		{"bad include glob", strings.Replace(minimal, `"version": 1,`, `"version": 1, "review": {"include": ["src/**x"]},`, 1), "review.include: pattern"},
		{"bad exclude glob", strings.Replace(minimal, `"version": 1,`, `"version": 1, "review": {"exclude": ["[ab"]},`, 1), "review.exclude: pattern"},
		{"bad reasoning effort", strings.Replace(minimal, `}}`, `}, "find": {"reasoning_effort": "extreme"}}`, 1), `models.find: reasoning_effort must be one of none, minimal, low, medium, high, xhigh, max (got "extreme")`},
		{"bad stage override", strings.Replace(minimal, `}}`, `}, "find": {"provider": "nope"}}`, 1), `models.find: unknown provider "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v\nwant it to contain %q", err, tt.want)
			}
		})
	}
}

func TestStageOverrideExplicitEmptyReplacesDefault(t *testing.T) {
	cfg, err := Parse([]byte(`{
	  "version": 1,
	  "models": {
	    "default": {"provider": "openai-compatible", "base_url": "https://llm.example", "model": "qwen3-coder", "api_key_env": "KEY", "max_output_tokens": 4096, "reasoning_effort": "high"},
	    "verify": {"provider": "ollama", "model": "glm4", "base_url": "", "api_key_env": "", "max_output_tokens": 0, "reasoning_effort": ""}
	  }
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := cfg.Stage(StageVerify)
	if v.BaseURL != "" || v.APIKeyEnv != "" || v.MaxOutputTokens != 0 || v.ReasoningEffort != "" {
		t.Errorf("explicit empty override fields were not applied: %+v", v)
	}
	if v.Provider != ProviderOllama || v.Model != "glm4" {
		t.Errorf("override fields = %+v", v)
	}
	if v.RequestTimeout != cfg.Models.Default.RequestTimeout {
		t.Errorf("absent request_timeout was not inherited: %v", v.RequestTimeout.Std())
	}
}

func TestZeroIterationsMeansNoCeiling(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(minimal, `"version": 1,`, `"version": 1, "limits": {"max_tool_iterations": 0},`, 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Limits.MaxToolIterations != 0 {
		t.Errorf("max_tool_iterations = %d, want 0", cfg.Limits.MaxToolIterations)
	}
}

func TestValidationReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`{"version": 1, "models": {"default": {"provider": "anthropic", "model": "m"}},
	  "limits": {"max_concurrency": 0}, "gate": {"fail_on": "x"}}`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if len(ve.Problems) != 3 {
		t.Errorf("got %d problems, want 3:\n%v", len(ve.Problems), err)
	}
}

func TestDefaultsAreIndependentCopies(t *testing.T) {
	a := Defaults()
	a.Review.Exclude[0] = "changed"
	if Defaults().Review.Exclude[0] == "changed" {
		t.Error("Defaults shares slice storage between calls")
	}
}

func TestRanks(t *testing.T) {
	for i, s := range []string{SeverityP0, SeverityP1, SeverityP2, SeverityP3} {
		if r, ok := SeverityRank(s); !ok || r != i {
			t.Errorf("SeverityRank(%s) = %d, %v", s, r, ok)
		}
	}
	for i, c := range []string{ConfidenceLow, ConfidenceMedium, ConfidenceHigh} {
		if r, ok := ConfidenceRank(c); !ok || r != i {
			t.Errorf("ConfidenceRank(%s) = %d, %v", c, r, ok)
		}
	}
	if _, ok := SeverityRank("P4"); ok {
		t.Error("SeverityRank accepted P4")
	}
	if _, ok := ConfidenceRank("sure"); ok {
		t.Error("ConfidenceRank accepted sure")
	}
}

// Package config loads and validates buggrit's repository configuration,
// .github/buggrit.json (docs/design.md §4).
//
// The file is read from the PR's base branch, never its head, so a PR cannot
// redirect the model endpoint. Decoding is strict: unknown keys are an error.
// Any setting the file leaves out keeps its code default; version and the
// default model are required.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// Path is where the config lives in a consuming repository.
const Path = ".github/buggrit.json"

// SupportedVersion is the only schema version this build understands.
const SupportedVersion = 1

// Provider names are buggrit's own, not maestro-llms package names, so the
// mapping onto a provider package can change without changing any config.
const (
	ProviderAnthropic        = "anthropic"
	ProviderOpenAI           = "openai"
	ProviderGoogle           = "google"
	ProviderOllama           = "ollama"
	ProviderOpenAICompatible = "openai-compatible"
)

// Limit profiles (§9). In unlimited, token and iteration settings are sanity
// ceilings against runaways; in budget they are hard spending caps.
const (
	ProfileUnlimited = "unlimited"
	ProfileBudget    = "budget"
)

// Severities (§5). P0 and P1 are blocking; P2 and P3 are advisory.
const (
	SeverityP0 = "P0"
	SeverityP1 = "P1"
	SeverityP2 = "P2"
	SeverityP3 = "P3"
)

// Confidence levels a finding may carry.
const (
	ConfidenceLow    = "low"
	ConfidenceMedium = "medium"
	ConfidenceHigh   = "high"
)

// GateNone disables the gate check (§8).
const GateNone = "none"

// Config is the whole repository configuration.
type Config struct {
	Models      Models      `json:"models"`
	Review      Review      `json:"review"`
	Identity    Identity    `json:"identity"`
	Gate        Gate        `json:"gate"`
	Limits      Limits      `json:"limits"`
	Convergence Convergence `json:"convergence"`
	Version     int         `json:"version"`
}

// Models holds the default model and optional per-stage overrides. A stage
// override changes only the fields it sets; see Stage.
type Models struct {
	Find    *ModelOverride `json:"find,omitempty"`
	Verify  *ModelOverride `json:"verify,omitempty"`
	Dedupe  *ModelOverride `json:"dedupe,omitempty"`
	Default Model          `json:"default"`
}

// Model selects one model on one provider.
type Model struct {
	Provider string `json:"provider,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Model    string `json:"model,omitempty"`
	// APIKeyEnv names the environment variable holding the key. The key
	// itself never appears in config.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// RequestTimeout bounds a single model call attempt; retries each get
	// their own.
	RequestTimeout Duration `json:"request_timeout,omitempty"`
	// MaxOutputTokens caps each response; 0 means the provider's default.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// ModelOverride is a per-stage change to the default model. Every field is a
// pointer so that a field present in the file, even as "" or 0, replaces the
// default, and only a field absent from the file is inherited.
type ModelOverride struct {
	Provider        *string   `json:"provider,omitempty"`
	BaseURL         *string   `json:"base_url,omitempty"`
	Model           *string   `json:"model,omitempty"`
	APIKeyEnv       *string   `json:"api_key_env,omitempty"`
	RequestTimeout  *Duration `json:"request_timeout,omitempty"`
	MaxOutputTokens *int      `json:"max_output_tokens,omitempty"`
}

// Limits bound a single pass (§9).
type Limits struct {
	Profile string `json:"profile"`
	// PassDeadline must sit below the job's timeout-minutes so a pass always
	// has time to publish partial results.
	PassDeadline   Duration `json:"pass_deadline"`
	MaxConcurrency int      `json:"max_concurrency"`
	// MaxToolIterations and MaxTokensPerPass of 0 mean no ceiling. Whatever
	// calls toolloop must translate 0 itself: toolloop treats MaxIterations
	// <= 0 as its own small default, not as unlimited.
	MaxToolIterations int `json:"max_tool_iterations"`
	MaxTokensPerPass  int `json:"max_tokens_per_pass"`
}

// Review controls what is reviewed and which findings are reported.
type Review struct {
	MinSeverity      string   `json:"min_severity"`
	MinConfidence    string   `json:"min_confidence"`
	Instructions     string   `json:"instructions"`
	Include          []string `json:"include"`
	Exclude          []string `json:"exclude"`
	InstructionFiles []string `json:"instruction_files"`
}

// Convergence holds the loop-ending knobs (§7).
type Convergence struct {
	MaxRebuttals  int `json:"max_rebuttals"`
	WarnAfterPass int `json:"warn_after_pass"`
}

// Gate configures the optional findings check run (§8).
type Gate struct {
	FailOn string `json:"fail_on"`
}

// Identity says which GitHub login is buggrit's own. A comment is buggrit's
// only if it carries the marker and is authored by this login (§3).
type Identity struct {
	BotLogin string `json:"bot_login"`
}

// Stage names a review stage that may have its own model.
type Stage string

// The stages that can override the default model.
const (
	StageFind   Stage = "find"
	StageVerify Stage = "verify"
	StageDedupe Stage = "dedupe"
)

// Stages lists every stage, in pipeline order.
func Stages() []Stage { return []Stage{StageFind, StageVerify, StageDedupe} }

// Defaults returns the configuration used for every setting a file leaves
// out. Models.Default has no default provider or model: those are required.
func Defaults() Config {
	return Config{
		Models: Models{
			Default: Model{RequestTimeout: Duration(10 * time.Minute)},
		},
		Limits: Limits{
			Profile:           ProfileUnlimited,
			PassDeadline:      Duration(30 * time.Minute),
			MaxConcurrency:    4,
			MaxToolIterations: 200,
		},
		Review: Review{
			MinSeverity:      SeverityP3,
			MinConfidence:    ConfidenceMedium,
			Include:          []string{"**"},
			Exclude:          []string{"**/*.lock", "go.sum", "**/*.pb.go", "vendor/**", "**/*_gen.go"},
			InstructionFiles: []string{"CLAUDE.md", "AGENTS.md", ".github/copilot-instructions.md"},
		},
		Convergence: Convergence{MaxRebuttals: 1, WarnAfterPass: 5},
		Gate:        Gate{FailOn: GateNone},
		Identity:    Identity{BotLogin: "buggrit[bot]"},
	}
}

// Parse decodes data over Defaults and validates the result.
func Parse(data []byte) (*Config, error) {
	cfg := Defaults()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode %s: %w", Path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode %s: unexpected data after the config object", Path)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Stage returns the effective model for a stage: the default model with the
// stage's override applied field by field.
func (c *Config) Stage(s Stage) Model {
	m := c.Models.Default
	o := c.override(s)
	if o == nil {
		return m
	}
	if o.Provider != nil {
		m.Provider = *o.Provider
	}
	if o.BaseURL != nil {
		m.BaseURL = *o.BaseURL
	}
	if o.Model != nil {
		m.Model = *o.Model
	}
	if o.APIKeyEnv != nil {
		m.APIKeyEnv = *o.APIKeyEnv
	}
	if o.RequestTimeout != nil {
		m.RequestTimeout = *o.RequestTimeout
	}
	if o.MaxOutputTokens != nil {
		m.MaxOutputTokens = *o.MaxOutputTokens
	}
	return m
}

func (c *Config) override(s Stage) *ModelOverride {
	switch s {
	case StageFind:
		return c.Models.Find
	case StageVerify:
		return c.Models.Verify
	case StageDedupe:
		return c.Models.Dedupe
	}
	return nil
}

// Validate reports every problem in the config at once, so a user fixes the
// file in one round rather than one error per run.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	switch c.Version {
	case 0:
		add("version is required (current: %d)", SupportedVersion)
	case SupportedVersion:
	default:
		add("version %d is not supported (this build supports %d)", c.Version, SupportedVersion)
	}

	if c.Models.Default.Provider == "" || c.Models.Default.Model == "" {
		add("models.default must set provider and model")
	}
	errs = append(errs, validateModel("models.default", &c.Models.Default)...)
	for _, s := range Stages() {
		if c.override(s) != nil {
			m := c.Stage(s)
			errs = append(errs, validateModel("models."+string(s), &m)...)
		}
	}

	errs = append(errs, c.Limits.validate()...)
	errs = append(errs, c.Review.validate()...)

	if c.Convergence.MaxRebuttals < 0 {
		add("convergence.max_rebuttals must be >= 0")
	}
	if c.Convergence.WarnAfterPass < 1 {
		add("convergence.warn_after_pass must be >= 1")
	}
	switch c.Gate.FailOn {
	case GateNone, SeverityP0, SeverityP1:
	default:
		add("gate.fail_on must be one of none, P0, P1 (got %q)", c.Gate.FailOn)
	}
	if strings.TrimSpace(c.Identity.BotLogin) == "" {
		add("identity.bot_login must not be empty")
	}

	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Problems: errs}
}

// envNameRE matches a portable environment variable name.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`) //nolint:gochecknoglobals // compiled once

func validateModel(field string, m *Model) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{field}, args...)...))
	}
	switch m.Provider {
	case "":
		return nil // reported once, against models.default
	case ProviderAnthropic, ProviderOpenAI, ProviderGoogle:
		if m.APIKeyEnv == "" {
			add("provider %s needs api_key_env", m.Provider)
		}
	case ProviderOpenAICompatible:
		if m.BaseURL == "" {
			add("provider %s needs base_url", m.Provider)
		}
	case ProviderOllama:
	default:
		add("unknown provider %q (want anthropic, openai, google, ollama or openai-compatible)", m.Provider)
	}
	if m.APIKeyEnv != "" && !envNameRE.MatchString(m.APIKeyEnv) {
		add("api_key_env %q is not a valid environment variable name", m.APIKeyEnv)
	}
	if m.BaseURL != "" && !strings.HasPrefix(m.BaseURL, "http://") && !strings.HasPrefix(m.BaseURL, "https://") {
		add("base_url %q must start with http:// or https://", m.BaseURL)
	}
	if m.RequestTimeout <= 0 {
		add("request_timeout must be positive")
	}
	if m.MaxOutputTokens < 0 {
		add("max_output_tokens must be >= 0")
	}
	return errs
}

func (l *Limits) validate() []error {
	var errs []error
	switch l.Profile {
	case ProfileUnlimited, ProfileBudget:
	default:
		errs = append(errs, fmt.Errorf("limits.profile must be unlimited or budget (got %q)", l.Profile))
	}
	if l.PassDeadline <= 0 {
		errs = append(errs, errors.New("limits.pass_deadline must be positive"))
	}
	if l.MaxConcurrency < 1 {
		errs = append(errs, errors.New("limits.max_concurrency must be >= 1"))
	}
	if l.MaxToolIterations < 0 {
		errs = append(errs, errors.New("limits.max_tool_iterations must be >= 0 (0 means no ceiling)"))
	}
	if l.MaxTokensPerPass < 0 {
		errs = append(errs, errors.New("limits.max_tokens_per_pass must be >= 0 (0 means no ceiling)"))
	}
	return errs
}

func (r *Review) validate() []error {
	var errs []error
	if _, ok := SeverityRank(r.MinSeverity); !ok {
		errs = append(errs, fmt.Errorf("review.min_severity must be one of P0, P1, P2, P3 (got %q)", r.MinSeverity))
	}
	switch r.MinConfidence {
	case ConfidenceLow, ConfidenceMedium, ConfidenceHigh:
	default:
		errs = append(errs, fmt.Errorf("review.min_confidence must be low, medium or high (got %q)", r.MinConfidence))
	}
	if len(r.Include) == 0 {
		errs = append(errs, errors.New("review.include must not be empty"))
	}
	for _, list := range [][]string{r.Include, r.Exclude, r.InstructionFiles} {
		for _, p := range list {
			if strings.TrimSpace(p) == "" {
				errs = append(errs, errors.New("review paths and patterns must not be empty strings"))
			}
		}
	}
	return errs
}

// SeverityRank orders severities from most severe (0, P0) to least (3, P3).
func SeverityRank(s string) (int, bool) {
	switch s {
	case SeverityP0:
		return 0, true
	case SeverityP1:
		return 1, true
	case SeverityP2:
		return 2, true
	case SeverityP3:
		return 3, true
	}
	return 0, false
}

// ValidationError lists every problem found in a config.
type ValidationError struct {
	Problems []error
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		msgs[i] = p.Error()
	}
	return "invalid " + Path + ":\n  - " + strings.Join(msgs, "\n  - ")
}

// Unwrap exposes the individual problems to errors.Is and errors.As.
func (e *ValidationError) Unwrap() []error { return e.Problems }

// Duration is a time.Duration written in JSON as a Go duration string such as
// "30m" or "1h30m".
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(time.Duration(d).String())
	if err != nil {
		return nil, fmt.Errorf("marshal duration: %w", err)
	}
	return b, nil
}

// UnmarshalJSON accepts only a duration string; a bare number is rejected
// because its unit would be ambiguous.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

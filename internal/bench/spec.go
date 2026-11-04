// Package bench is the benchmark harness's logic (docs/design.md §12,
// "Benchmarks"; docs/benchmarking.md): the benchmark and experiment files,
// config overlays, replay pull requests, per-run results read from
// telemetry, the judge that scores findings against known defects, and the
// report. cmd/buggrit-bench is its front end; the review engine never sees
// it.
package bench

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
)

// Target is the change a run reviews: head against base, in a repository.
// A replay PR is opened for it with the title and body.
type Target struct {
	Repo  string `json:"repo"`
	Base  string `json:"base"`
	Head  string `json:"head"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

func (t *Target) validate(field string) []error {
	var errs []error
	if _, err := github.ParseRepo(t.Repo); err != nil {
		errs = append(errs, fmt.Errorf("%s.repo: %w", field, err))
	}
	if !github.IsFullSHA(t.Base) {
		errs = append(errs, fmt.Errorf("%s.base must be a full commit SHA", field))
	}
	if !github.IsFullSHA(t.Head) {
		errs = append(errs, fmt.Errorf("%s.head must be a full commit SHA", field))
	}
	if strings.TrimSpace(t.Title) == "" {
		errs = append(errs, fmt.Errorf("%s.title is required", field))
	}
	return errs
}

// Benchmark is a target with its known defects, checked in as
// bench/<name>.json.
type Benchmark struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Target
	Defects []Defect `json:"defects"`
}

// Defect is one known defect in a benchmark's target.
type Defect struct {
	// ID is stable within the benchmark; scores refer to it.
	ID string `json:"id"`
	// Source is who raised it: copilot, buggrit, codex, a person.
	Source      string `json:"source"`
	Description string `json:"description"`
	// Locations are where it is, at the target's head. A class defect has
	// several.
	Locations []Location `json:"locations"`
}

// Location is a line range in a file at the target's head; Start and End
// of 0 mean the whole file.
type Location struct {
	Path  string `json:"path"`
	Start int    `json:"start,omitempty"`
	End   int    `json:"end,omitempty"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// The rules, as errors say them: nameRule for names (experiments, arms,
// benchmarks), which can be in branch names; idRule for defect IDs, which
// are plain slugs.
const (
	idRule   = "must be lowercase letters, digits, '.', '_' or '-', starting with a letter or digit"
	nameRule = idRule + ", without '..' or a trailing '.' or '.lock'"
)

// validName checks a name against nameRE and git's rules for a ref
// component, since an experiment's name is in its replay's branches: no
// "..", and no trailing "." or ".lock".
func validName(s string) bool {
	return nameRE.MatchString(s) && !strings.Contains(s, "..") && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, ".lock")
}

// ParseBenchmark decodes and validates a benchmark file strictly, and
// returns it with the sha256 of its bytes, which identifies it in the
// database.
func ParseBenchmark(data []byte) (*Benchmark, string, error) {
	var b Benchmark
	if err := strictDecode(data, &b); err != nil {
		return nil, "", fmt.Errorf("benchmark: %w", err)
	}
	var errs []error
	if !validName(b.Name) {
		errs = append(errs, errors.New("name "+nameRule))
	}
	errs = append(errs, b.validate("target")...)
	if len(b.Defects) == 0 {
		errs = append(errs, errors.New("defects: a benchmark needs at least one"))
	}
	errs = append(errs, validateDefects(b.Defects)...)
	if err := errors.Join(errs...); err != nil {
		return nil, "", fmt.Errorf("benchmark: %w", err)
	}
	sum := sha256.Sum256(data)
	return &b, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateDefects(defects []Defect) []error {
	var errs []error
	seen := map[string]bool{}
	for i := range defects {
		d := &defects[i]
		f := fmt.Sprintf("defects[%d]", i)
		switch {
		case !nameRE.MatchString(d.ID):
			errs = append(errs, fmt.Errorf("%s.id %s", f, idRule))
		case seen[d.ID]:
			errs = append(errs, fmt.Errorf("%s.id %q is repeated", f, d.ID))
		}
		seen[d.ID] = true
		if strings.TrimSpace(d.Description) == "" {
			errs = append(errs, fmt.Errorf("%s.description is required", f))
		}
		if strings.TrimSpace(d.Source) == "" {
			errs = append(errs, fmt.Errorf("%s.source is required", f))
		}
		if len(d.Locations) == 0 {
			errs = append(errs, fmt.Errorf("%s.locations: at least one", f))
		}
		for j := range d.Locations {
			l := &d.Locations[j]
			if l.Path == "" || l.Start < 0 || l.End < l.Start || (l.Start == 0) != (l.End == 0) {
				errs = append(errs, fmt.Errorf("%s.locations[%d]: a path, and start ≤ end, both set or both 0", f, j))
			}
		}
	}
	return errs
}

// Experiment is one benchmark run: arms × runs dry-run reviews of a target,
// read back from telemetry and, with a benchmark, scored by a judge.
type Experiment struct {
	// Judge scores findings against the benchmark's defects. It's required
	// with a benchmark and has no default, so every experiment says what
	// scored it.
	Judge *config.Model `json:"judge,omitempty"`
	// Target is the change to review when there's no benchmark.
	Target *Target `json:"target,omitempty"`
	Name   string  `json:"name"`
	// Benchmark is the path of a benchmark file in buggrit's checkout.
	Benchmark string `json:"benchmark,omitempty"`
	// BaseConfig is the config each arm's overlay applies to, a path in
	// buggrit's checkout; default .github/buggrit.json.
	BaseConfig string `json:"base_config,omitempty"`
	Arms       []Arm  `json:"arms"`
	// Runs per arm (default 3), and how many may be in flight at once
	// (default 2).
	Runs     int `json:"runs,omitempty"`
	Parallel int `json:"parallel,omitempty"`
}

// Arm is one variant: buggrit built from Ref, with Overlay applied to the
// base config.
type Arm struct {
	Name string `json:"name"`
	// Ref is the buggrit branch, tag or SHA to build; default main.
	Ref     string          `json:"ref,omitempty"`
	Overlay json.RawMessage `json:"overlay,omitempty"`
}

// Defaults for an experiment.
const (
	DefaultRuns       = 3
	DefaultParallel   = 2
	DefaultRef        = "main"
	DefaultBaseConfig = config.Path
	maxRuns           = 20
	// MaxJobs is GitHub Actions' limit on a matrix's jobs: an experiment
	// runs arms × runs review jobs in one matrix.
	MaxJobs = 256
)

// ParseExperiment decodes and validates an experiment strictly and fills
// in its defaults. Whether it has a benchmark or a target is checked here;
// the benchmark file itself is read by the caller.
func ParseExperiment(data []byte) (*Experiment, error) {
	var e Experiment
	if err := strictDecode(data, &e); err != nil {
		return nil, fmt.Errorf("experiment: %w", err)
	}
	if e.Runs == 0 {
		e.Runs = DefaultRuns
	}
	if e.Parallel == 0 {
		e.Parallel = DefaultParallel
	}
	if e.BaseConfig == "" {
		e.BaseConfig = DefaultBaseConfig
	}
	var errs []error
	if !validName(e.Name) {
		errs = append(errs, errors.New("name "+nameRule))
	}
	errs = append(errs, e.validateSubject()...)
	if e.Runs < 1 || e.Runs > maxRuns {
		errs = append(errs, fmt.Errorf("runs must be 1 to %d", maxRuns))
	}
	if e.Parallel < 1 {
		errs = append(errs, errors.New("parallel must be at least 1"))
	}
	errs = append(errs, e.validateArms()...)
	if n := len(e.Arms) * e.Runs; n > MaxJobs {
		errs = append(errs, fmt.Errorf("%d arms × %d runs is %d review jobs; a workflow's matrix allows %d", len(e.Arms), e.Runs, n, MaxJobs))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("experiment: %w", err)
	}
	return &e, nil
}

// validateSubject checks what's reviewed and how it's judged: a benchmark
// with its judge, or a target with none.
func (e *Experiment) validateSubject() []error {
	var errs []error
	switch {
	case e.Benchmark != "" && e.Target != nil:
		errs = append(errs, errors.New("give a benchmark or a target, not both"))
	case e.Benchmark == "" && e.Target == nil:
		errs = append(errs, errors.New("give a benchmark or a target"))
	case e.Target != nil:
		errs = append(errs, e.Target.validate("target")...)
	}
	switch {
	case e.Benchmark != "" && e.Judge == nil:
		errs = append(errs, errors.New("judge: a benchmark needs a judge; there's no default"))
	case e.Benchmark == "" && e.Judge != nil:
		errs = append(errs, errors.New("judge: only a benchmark is judged"))
	case e.Judge != nil:
		if _, err := JudgeConfig(e.Judge); err != nil {
			errs = append(errs, fmt.Errorf("judge: %w", err))
		}
	}
	return errs
}

// validateArms checks the arms and fills in their default ref.
func (e *Experiment) validateArms() []error {
	var errs []error
	if len(e.Arms) == 0 {
		errs = append(errs, errors.New("arms: at least one"))
	}
	seen := map[string]bool{}
	for i := range e.Arms {
		a := &e.Arms[i]
		if a.Ref == "" {
			a.Ref = DefaultRef
		}
		switch {
		case !validName(a.Name):
			errs = append(errs, fmt.Errorf("arms[%d].name %s", i, nameRule))
		case seen[a.Name]:
			errs = append(errs, fmt.Errorf("arms[%d].name %q is repeated", i, a.Name))
		}
		seen[a.Name] = true
		if strings.ContainsAny(a.Ref, " \t\n") || strings.HasPrefix(a.Ref, "-") {
			errs = append(errs, fmt.Errorf("arms[%d].ref %q is not a ref", i, a.Ref))
		}
	}
	return errs
}

// JudgeConfig is a config whose default model is the judge, with the code
// defaults a config file would get for every field it leaves out, and
// validated as one.
func JudgeConfig(m *config.Model) (*config.Config, error) {
	data, err := json.Marshal(map[string]any{
		"version": config.SupportedVersion,
		"models":  map[string]any{"default": m},
		"limits":  map[string]any{"max_concurrency": 1},
	})
	if err != nil {
		return nil, fmt.Errorf("encode the judge: %w", err)
	}
	return config.Parse(data) //nolint:wrapcheck // the caller says it's the judge
}

// ParseRecordedExperiment parses an experiment as the database recorded
// it: like ParseExperiment, but a retired field is dropped rather than
// rejected, so an older experiment's report can still be regenerated. The
// retired fields are ones an earlier harness accepted and this one
// doesn't: "issue" named an issue for the report's comment, which the
// database and the job summary replaced. A new experiment naming one is
// rejected. Keys match case-insensitively, as encoding/json matched the
// old field, so "Issue" was accepted then and is dropped now.
func ParseRecordedExperiment(data []byte) (*Experiment, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("experiment: %w", err)
	}
	dropped := false
	for k := range fields {
		if strings.EqualFold(k, "issue") {
			delete(fields, k)
			dropped = true
		}
	}
	if !dropped {
		return ParseExperiment(data)
	}
	trimmed, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("experiment: %w", err)
	}
	return ParseExperiment(trimmed)
}

// Label is the telemetry label of an arm's runs: the experiment's name and
// the arm's. Rerunning an experiment adds to the same groups.
func (e *Experiment) Label(arm string) string { return e.Name + "/" + arm }

func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err //nolint:wrapcheck // the caller names the file
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the object")
	}
	return nil
}

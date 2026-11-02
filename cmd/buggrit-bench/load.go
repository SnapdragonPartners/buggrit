package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/buggrit/internal/bench"
	"github.com/SnapdragonPartners/buggrit/internal/config"
	"github.com/SnapdragonPartners/buggrit/internal/github"
	"github.com/SnapdragonPartners/buggrit/internal/llm"
	"github.com/SnapdragonPartners/buggrit/internal/store"
)

// deps are the command's seams to the outside world, replaced in tests.
type deps struct {
	github    func(token, apiURL string) *github.Client
	replayer  func(gh *github.Client) bench.Replayer
	openStore func(ctx context.Context, url string) (benchDB, error)
	judge     func(m *config.Model, getenv func(string) string) (llms.ChatClient, error)
	newID     func() string
}

// benchDB is the database, which the caller closes.
type benchDB interface {
	bench.Store
	Close() error
}

func defaultDeps() deps {
	return deps{
		github: func(token, apiURL string) *github.Client {
			return github.NewClient(token, github.WithBaseURL(apiURL))
		},
		replayer: func(gh *github.Client) bench.Replayer { return &bench.GitHubReplayer{GH: gh} },
		openStore: func(ctx context.Context, url string) (benchDB, error) {
			s, err := store.OpenBench(ctx, url)
			if err != nil {
				return nil, err //nolint:wrapcheck // the message is shown as is
			}
			return storeDB{Store: s.Bench(), s: s}, nil
		},
		judge: newJudge,
		newID: newUUID,
	}
}

type storeDB struct {
	bench.Store
	s *store.Store
}

func (d storeDB) Close() error { return d.s.Close() } //nolint:wrapcheck // closing never fails

// newJudge builds the judge's client the way a review builds a stage's,
// with the same retries, timeout and reasoning settings.
func newJudge(m *config.Model, getenv func(string) string) (llms.ChatClient, error) {
	cfg, err := bench.JudgeConfig(m)
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	c, err := llm.New(cfg, llm.WithGetenv(getenv))
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	return c.For(config.StageFind), nil
}

// loaded is an experiment with everything it names, read and checked.
type loaded struct {
	exp *bench.Experiment
	// bm and hash are nil and "" without a benchmark.
	bm *bench.Benchmark
	// configs are the arms' merged configs, by arm name, and timeouts
	// their review jobs' timeouts in minutes: the pass deadline plus
	// time to start and publish.
	configs  map[string][]byte
	timeouts map[string]int
	hash     string
	spec     []byte
}

// target is what the experiment reviews.
func (l *loaded) target() *bench.Target {
	if l.bm != nil {
		return &l.bm.Target
	}
	return l.exp.Target
}

// databaseURL is the database's URL from the environment. The workflow's
// database action sets BUGGRIT_DATABASE_ERROR when it couldn't connect;
// that reason is the error to show.
func databaseURL(getenv func(string) string) (string, error) {
	if e := getenv("BUGGRIT_DATABASE_ERROR"); e != "" {
		return "", fmt.Errorf("the database couldn't be set up: %s", e)
	}
	url := getenv("BUGGRIT_DATABASE_URL")
	if url == "" {
		return "", usagef("BUGGRIT_DATABASE_URL is not set; the results are recorded in the database")
	}
	return url, nil
}

// jobOverhead is what a review job needs beyond its pass deadline: setup,
// the build and publishing.
const jobOverhead = 15 * time.Minute

// The report job judges one run at a time, so its timeout grows with the
// runs: reportBase for setup and the report, plus reportPerRun for each
// judgment (up to three model calls), capped at maxJobMinutes, a hosted
// runner's limit. Every score is saved as it's made, so a report cut off
// at the cap keeps what it judged; `report` run again judges the rest.
const (
	reportBase    = 10
	reportPerRun  = 3
	maxJobMinutes = 360
)

// reportTimeout is the report job's timeout-minutes.
func (l *loaded) reportTimeout() int {
	if l.bm == nil {
		return reportBase
	}
	return min(reportBase+reportPerRun*len(l.exp.Arms)*l.exp.Runs, maxJobMinutes)
}

// load reads an experiment file and, from root (buggrit's checkout), its
// benchmark and base config, and merges each arm's overlay.
func load(root, path string) (*loaded, error) {
	spec, err := readFile(path)
	if err != nil {
		return nil, err
	}
	exp, err := bench.ParseExperiment(spec)
	if err != nil {
		return nil, err //nolint:wrapcheck // says it's the experiment
	}
	l := &loaded{exp: exp, spec: spec, configs: map[string][]byte{}, timeouts: map[string]int{}}
	if exp.Benchmark != "" {
		data, rerr := readUnder(root, exp.Benchmark)
		if rerr != nil {
			return nil, rerr
		}
		if l.bm, l.hash, err = bench.ParseBenchmark(data); err != nil {
			return nil, fmt.Errorf("%s: %w", exp.Benchmark, err)
		}
	}
	base, err := readUnder(root, exp.BaseConfig)
	if err != nil {
		return nil, err
	}
	for i := range exp.Arms {
		a := &exp.Arms[i]
		merged, err := bench.Merge(base, a.Overlay)
		if err != nil {
			return nil, fmt.Errorf("arm %s: %w", a.Name, err)
		}
		cfg, err := config.Parse(merged)
		if err != nil {
			return nil, fmt.Errorf("arm %s: %w", a.Name, err)
		}
		l.configs[a.Name] = merged
		l.timeouts[a.Name] = int((cfg.Limits.PassDeadline.Std() + jobOverhead + time.Minute - 1) / time.Minute)
	}
	return l, nil
}

// readUnder reads a path the experiment names, which must stay inside
// root.
func readUnder(root, rel string) ([]byte, error) {
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%s: must be a relative path inside buggrit's checkout", rel)
	}
	return readFile(filepath.Join(root, rel))
}

func readFile(path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read the experiment from stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // the operator's own path
	if err != nil {
		return nil, err //nolint:wrapcheck // names the file
	}
	return data, nil
}

// runCheck validates an experiment without opening anything.
func runCheck(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "buggrit's checkout, which the experiment's paths are relative to")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() != 1 {
		return usagef("check takes one experiment file")
	}
	l, err := load(*root, fs.Arg(0))
	if err != nil {
		return err
	}
	t := l.target()
	say(stdout, "experiment %s: %d arm(s) × %d run(s), %d at a time, on %s %s...%s\n",
		l.exp.Name, len(l.exp.Arms), l.exp.Runs, l.exp.Parallel, t.Repo, short(t.Base), short(t.Head))
	if l.bm != nil {
		say(stdout, "benchmark %s: %d known defects (%s); judge %s %s\n", l.bm.Name, len(l.bm.Defects), l.hash, l.exp.Judge.Provider, l.exp.Judge.Model)
	}
	say(stdout, "report timeout %dm\n", l.reportTimeout())
	for i := range l.exp.Arms {
		a := &l.exp.Arms[i]
		say(stdout, "arm %s: ref %s, label %s, job timeout %dm\n", a.Name, a.Ref, l.exp.Label(a.Name), l.timeouts[a.Name])
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// newUUID is a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

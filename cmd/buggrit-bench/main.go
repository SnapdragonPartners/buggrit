// Command buggrit-bench runs benchmark experiments: arms × runs of dry-run
// reviews of a replayed pull request, scored from their telemetry (design
// §12, "Benchmarks"; docs/benchmarking.md). The bench workflow
// (.github/workflows/bench.yml) runs prepare, the reviews, report and
// cleanup; report also runs locally to regenerate a report.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const usage = `usage: buggrit-bench <command> [flags]

commands:
  check [--root dir] <experiment.json>
                            validate an experiment, its benchmark and every
                            arm's merged config, without opening anything
  prepare [flags]           validate an experiment, open its replay PR and
                            write the review matrix (in the bench workflow)
  report [flags]            score an experiment's runs and write the report;
                            with --workflow-run, regenerate it from the
                            database, overrides applied
  cleanup [flags]           close the replay PR and delete its branches

Run "buggrit-bench <command> -h" for a command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, defaultDeps(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, d deps, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		say(stderr, "%s", usage)
		return 2
	}
	var err error
	switch args[0] {
	case "check":
		err = runCheck(args[1:], stdout, stderr)
	case "prepare":
		err = runPrepare(ctx, d, args[1:], getenv, stdout, stderr)
	case "report":
		err = runReport(ctx, d, args[1:], getenv, stdout, stderr)
	case "cleanup":
		err = runCleanup(ctx, d, args[1:], getenv, stderr)
	case "help", "-h", "--help":
		say(stdout, "%s", usage)
		return 0
	default:
		say(stderr, "buggrit-bench: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	var ue usageError
	switch {
	case errors.As(err, &ue):
		say(stderr, "buggrit-bench %s: %v\n", args[0], err)
		return 2
	case err != nil:
		say(stderr, "buggrit-bench %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// usageError is a bad command line.
type usageError struct{ error }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// Command buggrit is an automated pull-request reviewer that runs as a
// GitHub Action. See docs/design.md.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/SnapdragonPartners/buggrit/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: buggrit <command> [arguments]

commands:
  review [flags]           review a pull request and post the findings; in an
                           Action, every flag defaults from the environment
                           (run "buggrit review -h" for the flags)
  config validate [path]   validate a config file (default ` + config.Path + `)
                           and print it with defaults applied
  db migrate [flags]       bring the optional database's schemas up to date
                           (run as the role that owns them)
  db status [flags]        show each schema's version
  version                  print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without process exit, so it can be tested.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		say(stderr, "%s", usage)
		return 2
	}
	switch args[0] {
	case "version":
		say(stdout, "%s\n", version)
		return 0
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "db":
		// No signalContext: golang-migrate waits for its advisory lock
		// without a context, so Ctrl-C has to end the process. The lock
		// goes with the connection.
		return runDB(context.Background(), defaultDBOps(), args[1:], os.Getenv, stdout, stderr)
	case "review":
		ctx, stop := signalContext()
		defer stop()
		return runReview(ctx, args[1:], os.Getenv, stdout, stderr)
	case "help", "-h", "--help":
		say(stdout, "%s", usage)
		return 0
	}
	say(stderr, "buggrit: unknown command %q\n\n%s", args[0], usage)
	return 2
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "validate" || len(args) > 2 {
		say(stderr, "%s", usage)
		return 2
	}
	path := config.Path
	if len(args) == 2 {
		path = args[1]
	}
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		say(stderr, "buggrit: %v\n", err)
		return 1
	}
	cfg, err := config.Parse(data)
	if err != nil {
		say(stderr, "buggrit: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		say(stderr, "buggrit: %v\n", err)
		return 1
	}
	return 0
}

// say writes a message for the operator. A failed write to stdout or stderr
// has nowhere better to be reported, so the error is dropped.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

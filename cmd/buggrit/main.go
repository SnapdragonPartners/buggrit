// Command buggrit is an automated pull-request reviewer that runs as a
// GitHub Action. See docs/design.md.
package main

import (
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
  review                   review the pull request in the current Action run (not yet implemented)
  config validate [path]   validate a config file (default ` + config.Path + `)
                           and print it with defaults applied
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
	case "review":
		say(stderr, "buggrit review: not implemented yet (build step 3, docs/design.md §12)\n")
		return 1
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

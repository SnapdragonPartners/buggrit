package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/SnapdragonPartners/buggrit/internal/store"
)

// dbOps are the db command's database calls, replaced in tests.
type dbOps struct {
	migrate func(ctx context.Context, url string, report func(before, after store.SchemaStatus)) error
	status  func(ctx context.Context, url string) ([]store.SchemaStatus, error)
}

func defaultDBOps() dbOps { return dbOps{migrate: store.Migrate, status: store.Status} }

// runDB runs `buggrit db migrate|status`, which an operator runs as the
// role that owns the schemas; review runs never run DDL (design §6).
func runDB(ctx context.Context, ops dbOps, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "migrate" && args[0] != "status") {
		say(stderr, "%s", usage)
		return 2
	}
	fs := flag.NewFlagSet("db "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	// No default from the environment here: flag help would print it, and
	// the URL can hold a password.
	url := fs.String("database-url", "", "database URL (default $BUGGRIT_DATABASE_URL)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		say(stderr, "buggrit db: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *url == "" {
		*url = getenv("BUGGRIT_DATABASE_URL")
	}
	if *url == "" {
		say(stderr, "buggrit db: no database: set --database-url or BUGGRIT_DATABASE_URL\n")
		return 2
	}

	if args[0] == "status" {
		st, err := ops.status(ctx, *url)
		if err != nil {
			say(stderr, "buggrit db status: %v\n", err)
			return 1
		}
		for i := range st {
			say(stdout, "%s: %s\n", st[i].Name, describe(&st[i]))
		}
		return 0
	}
	err := ops.migrate(ctx, *url, func(before, after store.SchemaStatus) {
		switch {
		case after.Version == before.Version:
			say(stdout, "%s: %s, nothing to do\n", after.Name, describe(&after))
		default:
			say(stdout, "%s: migrated from %s to v%d\n", after.Name, schemaVersion(before.Version), after.Version)
		}
	})
	if err != nil {
		say(stderr, "buggrit db migrate: %v\n", err)
		return 1
	}
	return 0
}

// describe says where a schema stands against what this binary needs.
func describe(s *store.SchemaStatus) string {
	v := schemaVersion(s.Version)
	switch {
	case s.Dirty:
		return v + ", dirty: a migration failed partway"
	case s.Version < s.Required:
		return fmt.Sprintf("%s, this binary needs v%d: run `buggrit db migrate`", v, s.Required)
	case s.Version > s.Required:
		return fmt.Sprintf("%s, newer than this binary's v%d (fine)", v, s.Required)
	}
	return v + ", up to date"
}

func schemaVersion(v int) string {
	if v == 0 {
		return "none"
	}
	return fmt.Sprintf("v%d", v)
}

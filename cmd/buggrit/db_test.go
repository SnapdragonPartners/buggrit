package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/buggrit/internal/store"
)

func runDBWith(ops dbOps, env map[string]string, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = runDB(context.Background(), ops, args[1:], func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestDBMigrate(t *testing.T) {
	var gotURL string
	ops := dbOps{migrate: func(_ context.Context, url string, report func(before, after store.SchemaStatus)) error {
		gotURL = url
		report(store.SchemaStatus{Name: "bucket", Version: 1, Required: 1}, store.SchemaStatus{Name: "bucket", Version: 1, Required: 1})
		report(store.SchemaStatus{Name: "buggrit", Required: 1}, store.SchemaStatus{Name: "buggrit", Version: 1, Required: 1})
		return nil
	}}
	code, out, errOut := runDBWith(ops, map[string]string{"BUGGRIT_DATABASE_URL": "postgres://env"}, "db", "migrate")
	if code != 0 || gotURL != "postgres://env" {
		t.Fatalf("exit %d, url %q, stderr %s", code, gotURL, errOut)
	}
	want := "bucket: v1, up to date, nothing to do\nbuggrit: migrated from none to v1\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}

	// The flag beats the environment.
	runDBWith(ops, map[string]string{"BUGGRIT_DATABASE_URL": "postgres://env"}, "db", "migrate", "--database-url", "postgres://flag")
	if gotURL != "postgres://flag" {
		t.Errorf("url = %q", gotURL)
	}
}

func TestDBStatus(t *testing.T) {
	ops := dbOps{status: func(context.Context, string) ([]store.SchemaStatus, error) {
		return []store.SchemaStatus{
			{Name: "bucket", Version: 2, Required: 1},
			{Name: "buggrit", Version: 1, Required: 2},
			{Name: "dirty", Version: 3, Dirty: true, Required: 3},
		}, nil
	}}
	code, out, _ := runDBWith(ops, nil, "db", "status", "--database-url", "postgres://x")
	want := "bucket: v2, newer than this binary's v1 (fine)\n" +
		"buggrit: v1, this binary needs v2: run `buggrit db migrate`\n" +
		"dirty: v3, dirty: a migration failed partway\n"
	if code != 0 || out != want {
		t.Errorf("exit %d, stdout = %q", code, out)
	}
}

func TestDBErrors(t *testing.T) {
	failing := dbOps{
		migrate: func(context.Context, string, func(_, _ store.SchemaStatus)) error { return errors.New("boom") },
		status:  func(context.Context, string) ([]store.SchemaStatus, error) { return nil, errors.New("boom") },
	}
	for _, tc := range []struct {
		args []string
		want string
		code int
	}{
		{[]string{"db"}, "usage", 2},
		{[]string{"db", "drop"}, "usage", 2},
		{[]string{"db", "migrate"}, "no database", 2},
		{[]string{"db", "migrate", "--database-url", "x", "extra"}, "unexpected arguments", 2},
		{[]string{"db", "migrate", "--database-url", "x"}, "buggrit db migrate: boom", 1},
		{[]string{"db", "status", "--database-url", "x"}, "buggrit db status: boom", 1},
	} {
		code, _, errOut := runDBWith(failing, nil, tc.args...)
		if code != tc.code || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit %d, stderr %q", tc.args, code, errOut)
		}
	}
}

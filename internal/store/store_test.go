package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	bucketpg "github.com/SnapdragonPartners/maestro-llms/bucket/postgres"
)

// The Postgres tests run against a real server and skip unless
// BUGGRIT_POSTGRES_DSN is set to a URL-form DSN for a role that may create
// databases and roles. CI uses a service container. Each test creates its
// own database and drops it afterwards. Locally, for example:
//
//	docker run -d --name buggrit-pg -e POSTGRES_PASSWORD=buggrit -p 127.0.0.1:55432:5432 postgres:18
//	BUGGRIT_POSTGRES_DSN='postgres://postgres:buggrit@127.0.0.1:55432/postgres?sslmode=disable' make test

func TestMigrationsMatchSchemaVersion(t *testing.T) {
	ups, err := fs.Glob(Migrations(), "*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`^(\d{6})_[a-z0-9_]+\.up\.sql$`)
	highest := 0
	for _, up := range ups {
		m := name.FindStringSubmatch(up)
		if m == nil {
			t.Fatalf("%s: not named NNNNNN_name.up.sql", up)
		}
		n, _ := strconv.Atoi(m[1])
		highest = max(highest, n)
		if _, err := fs.Stat(Migrations(), strings.TrimSuffix(up, ".up.sql")+".down.sql"); err != nil {
			t.Errorf("%s has no down migration", up)
		}
		body, _ := fs.ReadFile(Migrations(), up)
		want := fmt.Sprintf("INSERT INTO buggrit.schema_version (version) VALUES (%d);", n)
		if !strings.HasSuffix(strings.TrimSpace(string(body)), want) {
			t.Errorf("%s must end with %q", up, want)
		}
	}
	if highest != SchemaVersion {
		t.Fatalf("SchemaVersion = %d but the highest migration is %d", SchemaVersion, highest)
	}
}

func TestSchemaErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		err  *SchemaError
		want string
	}{
		{&SchemaError{Schema: "buggrit", Required: 2}, "the buggrit schema must be v2 or newer, found none; run `buggrit db migrate`"},
		{&SchemaError{Schema: "bucket", Required: 2, Found: 1}, "the bucket schema must be v2 or newer, found v1; run `buggrit db migrate`"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// newDatabase creates an empty database and returns its URL. It skips the
// test when no server is configured.
func newDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("BUGGRIT_POSTGRES_DSN")
	if admin == "" {
		// CI sets BUGGRIT_POSTGRES_REQUIRED, so a broken service container
		// fails instead of skipping every Postgres test.
		if os.Getenv("BUGGRIT_POSTGRES_REQUIRED") != "" {
			t.Fatal("BUGGRIT_POSTGRES_REQUIRED is set but BUGGRIT_POSTGRES_DSN is empty")
		}
		t.Skip("BUGGRIT_POSTGRES_DSN not set")
	}
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("buggrit_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err = db.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		_ = db.Close()
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func exec(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func migrateAll(t *testing.T, dsn string) []SchemaStatus {
	t.Helper()
	var got []SchemaStatus
	if err := Migrate(context.Background(), dsn, func(_, after SchemaStatus) { got = append(got, after) }); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOpenNeedsMigrations(t *testing.T) {
	dsn := newDatabase(t)
	ctx := context.Background()
	var se *SchemaError
	if _, err := Open(ctx, dsn); !errors.As(err, &se) || se.Schema != "buggrit" || se.Found != 0 {
		t.Fatalf("Open before migrating: %v", err)
	}

	got := migrateAll(t, dsn)
	want := []SchemaStatus{
		{Name: "bucket", Version: bucketpg.SchemaVersion, Required: bucketpg.SchemaVersion},
		{Name: "buggrit", Version: SchemaVersion, Required: SchemaVersion},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Migrate = %+v, want %+v", got, want)
	}
	if st, err := Status(ctx, dsn); err != nil || fmt.Sprint(st) != fmt.Sprint(want) {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Migrating again changes nothing.
	if got := migrateAll(t, dsn); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("second Migrate = %+v", got)
	}
}

func TestStatusBeforeMigrating(t *testing.T) {
	dsn := newDatabase(t)
	st, err := Status(context.Background(), dsn)
	if err != nil || len(st) != 2 || st[0].Version != 0 || st[1].Version != 0 {
		t.Fatalf("Status = %+v, %v", st, err)
	}
}

// An older binary keeps working against a newer schema, and its migrate
// leaves the newer schema alone.
func TestNewerSchemaIsFine(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	next := SchemaVersion + 1
	exec(t, dsn,
		fmt.Sprintf("INSERT INTO buggrit.schema_version (version) VALUES (%d)", next),
		fmt.Sprintf("UPDATE buggrit.schema_migrations SET version = %d", next))
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open against a newer schema: %v", err)
	}
	_ = s.Close()
	if got := migrateAll(t, dsn); got[1].Version != next {
		t.Fatalf("Migrate touched a newer schema: %+v", got)
	}
}

func TestOpenOlderSchema(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	exec(t, dsn, "DELETE FROM buggrit.schema_version")
	var se *SchemaError
	if _, err := Open(context.Background(), dsn); !errors.As(err, &se) || se.Schema != "buggrit" {
		t.Fatalf("Open = %v", err)
	}
}

func TestOpenMissingBucketSchema(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	exec(t, dsn, "DROP SCHEMA bucket CASCADE")
	var se *SchemaError
	if _, err := Open(context.Background(), dsn); !errors.As(err, &se) || se.Schema != "bucket" || se.Found != 0 {
		t.Fatalf("Open = %v", err)
	}
}

func TestMigrateRefusesDirty(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	exec(t, dsn, "UPDATE buggrit.schema_migrations SET dirty = true")
	err := Migrate(context.Background(), dsn, func(_, _ SchemaStatus) {})
	if err == nil || !strings.Contains(err.Error(), "failed partway") {
		t.Fatalf("Migrate = %v", err)
	}
}

// The runtime role has DML only (design §10): it can open the store, and
// the check needs nothing more.
func TestOpenAsDMLOnlyRole(t *testing.T) {
	dsn := newDatabase(t)
	migrateAll(t, dsn)
	role := fmt.Sprintf("buggrit_runtime_%d", time.Now().UnixNano())
	exec(t, dsn,
		"CREATE ROLE "+role+" LOGIN PASSWORD 'runtime'",
		"GRANT USAGE ON SCHEMA buggrit, bucket TO "+role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA buggrit, bucket TO "+role)
	t.Cleanup(func() {
		exec(t, dsn, "DROP OWNED BY "+role, "DROP ROLE "+role)
	})
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword(role, "runtime")
	s, err := Open(context.Background(), u.String())
	if err != nil {
		t.Fatalf("Open as the runtime role: %v", err)
	}
	_ = s.Close()
	if err := Migrate(context.Background(), u.String(), func(_, _ SchemaStatus) {}); err == nil {
		t.Fatal("the runtime role could migrate")
	}
}

func TestOpenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Open(ctx, "postgres://nobody@127.0.0.1:1/buggrit?sslmode=disable&connect_timeout=2")
	var se *SchemaError
	if err == nil || errors.As(err, &se) {
		t.Fatalf("Open = %v", err)
	}
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	bucketpg "github.com/SnapdragonPartners/maestro-llms/bucket/postgres"
	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// schema is one set of migrations, applied to its own Postgres schema with
// its own golang-migrate table.
type schema struct {
	files    func() fs.FS
	name     string
	required int
}

// schemas returns the schemas in the order they're applied: the bucket
// schema first, since it doesn't depend on buggrit's.
func schemas() []schema {
	return []schema{
		{name: "bucket", files: bucketpg.Migrations, required: bucketpg.SchemaVersion},
		{name: "buggrit", files: Migrations, required: SchemaVersion},
	}
}

// SchemaStatus is one schema's migration state.
type SchemaStatus struct {
	// Name is the Postgres schema.
	Name string
	// Version is the last migration golang-migrate applied; 0 for none.
	// Dirty means that migration failed partway.
	Version int
	Dirty   bool
	// Required is the version this binary needs.
	Required int
}

// Migrate brings each schema up to this binary's latest migration. A
// schema that is already newer is left alone, so an older binary never
// rolls one back. A dirty schema, from a migration that failed partway, is
// an error: it needs fixing by hand. report is called once per schema with
// its state before and after.
func Migrate(ctx context.Context, url string, report func(before, after SchemaStatus)) error {
	all := schemas()
	for i := range all {
		before, after, err := migrateOne(ctx, url, &all[i])
		if err != nil {
			return fmt.Errorf("migrate the %s schema: %w", all[i].name, err)
		}
		report(before, after)
	}
	return nil
}

func migrateOne(ctx context.Context, url string, s *schema) (before, after SchemaStatus, err error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return before, after, err //nolint:wrapcheck // Migrate names the schema
	}
	// golang-migrate keeps its table in the schema, so the schema comes
	// first.
	if _, err = db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{s.name}.Sanitize()); err != nil {
		_ = db.Close()
		return before, after, err //nolint:wrapcheck // Migrate names the schema
	}
	// The driver closes db when m closes.
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{
		MigrationsTable:       pgx.Identifier{s.name, "schema_migrations"}.Sanitize(),
		MigrationsTableQuoted: true,
	})
	if err != nil {
		_ = db.Close()
		return before, after, err //nolint:wrapcheck // Migrate names the schema
	}
	src, err := iofs.New(s.files(), ".")
	if err != nil {
		_ = driver.Close()
		return before, after, err //nolint:wrapcheck // Migrate names the schema
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		_ = src.Close()
		_ = driver.Close()
		return before, after, err //nolint:wrapcheck // Migrate names the schema
	}
	defer func() { _, _ = m.Close() }()

	before, err = version(m, s)
	switch {
	case err != nil:
		return before, after, err
	case before.Dirty:
		return before, after, fmt.Errorf("v%d failed partway; fix the schema by hand, then set dirty to false in %s.schema_migrations", before.Version, s.name)
	case before.Version >= s.required:
		// Up to date, or newer than this binary knows.
		return before, before, nil
	}
	if err = m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return before, after, err //nolint:wrapcheck // Migrate names the schema
	}
	after, err = version(m, s)
	return before, after, err
}

func version(m *migrate.Migrate, s *schema) (SchemaStatus, error) {
	st := SchemaStatus{Name: s.name, Required: s.required}
	v, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		return st, nil
	case err != nil:
		return st, err //nolint:wrapcheck // Migrate names the schema
	}
	st.Version, st.Dirty = int(v), dirty //nolint:gosec // migration numbers are small
	return st, nil
}

// Status reads each schema's migration state without changing anything.
func Status(ctx context.Context, url string) ([]SchemaStatus, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err //nolint:wrapcheck // pgx's message is the one to show
	}
	defer func() { _ = db.Close() }()
	all := schemas()
	out := make([]SchemaStatus, 0, len(all))
	for i := range all {
		s := &all[i]
		st := SchemaStatus{Name: s.name, Required: s.required}
		table := pgx.Identifier{s.name, "schema_migrations"}.Sanitize()
		err := db.QueryRowContext(ctx, "SELECT version, dirty FROM "+table+" LIMIT 1").Scan(&st.Version, &st.Dirty)
		switch {
		case errors.Is(err, sql.ErrNoRows), isSchemaMissing(err):
			// Never migrated.
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", table, err)
		}
		out = append(out, st)
	}
	return out, nil
}

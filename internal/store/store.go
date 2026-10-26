// Package store is buggrit's optional database (docs/design.md §6, "The
// optional database"). A run uses it only when BUGGRIT_DATABASE_URL is set,
// and never for review state: the PR's threads stay the only record of a
// review. The engine never sees it; later work puts small interfaces (a
// limiter, a telemetry sink) in front of it.
//
// The database holds two schemas, each with its own golang-migrate table:
// buggrit's own, from this package's migrations, and maestro-llms' bucket
// schema for the shared rate limit. `buggrit db migrate` applies both;
// review runs never run DDL, and their role has DML only.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	bucketpg "github.com/SnapdragonPartners/maestro-llms/bucket/postgres"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	storedb "github.com/SnapdragonPartners/buggrit/internal/store/db"
)

// SchemaVersion is the buggrit schema version this binary requires. A test
// keeps it equal to the highest migration. A newer schema is fine, so an
// older pinned binary keeps working after a migration.
const SchemaVersion = 1

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns buggrit's schema migrations, in golang-migrate's format.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		// The embed pattern guarantees the directory exists.
		panic(fmt.Sprintf("store: embedded migrations: %v", err))
	}
	return sub
}

// Store is an open database whose schemas are new enough.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to url and checks that both schemas are at least the
// versions this binary requires. A schema that is missing or older is a
// *SchemaError; any other error is the connection's or the query's, as is.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err //nolint:wrapcheck // pgx's message is the one to show
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err //nolint:wrapcheck // pgx's message is the one to show
	}
	if err := checkSchemas(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// Close releases the store's connections.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

func checkSchemas(ctx context.Context, pool *pgxpool.Pool) error {
	found, err := storedb.New(pool).SchemaVersion(ctx)
	switch {
	case isSchemaMissing(err):
		return &SchemaError{Schema: "buggrit", Required: SchemaVersion, Cause: err}
	case err != nil:
		return fmt.Errorf("check the buggrit schema: %w", err)
	case found < SchemaVersion:
		return &SchemaError{Schema: "buggrit", Required: SchemaVersion, Found: int(found)}
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	return bucketSchema(ctx, db)
}

// bucketSchema checks maestro-llms' bucket schema, which the shared rate
// limit needs.
func bucketSchema(ctx context.Context, db *sql.DB) error {
	err := bucketpg.CheckSchema(ctx, db)
	var se *bucketpg.SchemaError
	if errors.As(err, &se) {
		return &SchemaError{Schema: "bucket", Required: se.Required, Found: max(se.Found, 0), Cause: se.Cause}
	}
	return err //nolint:wrapcheck // CheckSchema says what it was checking
}

// SchemaError means a schema is missing or older than this binary needs.
type SchemaError struct {
	// Cause is the database error, when there is one.
	Cause error
	// Schema is "buggrit" or "bucket".
	Schema string
	// Required is the version this binary needs; Found is the database's,
	// 0 when the schema is missing.
	Required, Found int
}

// Error implements error.
func (e *SchemaError) Error() string {
	found := fmt.Sprintf("found v%d", e.Found)
	if e.Found == 0 {
		found = "found none"
	}
	return fmt.Sprintf("the %s schema must be v%d or newer, %s; run `buggrit db migrate`", e.Schema, e.Required, found)
}

// Unwrap returns the database error.
func (e *SchemaError) Unwrap() error { return e.Cause }

// isSchemaMissing reports an undefined table, column or schema.
func isSchemaMissing(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return false
	}
	switch pg.Code {
	case "42P01", "42703", "3F000": // undefined_table, undefined_column, invalid_schema_name
		return true
	}
	return false
}

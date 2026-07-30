// Package store is the SQLite persistence layer.
//
// It uses database/sql directly with hand-written SQL rather than an ORM: the
// queries here are few, the schema is small, and having the exact SQL visible
// at the call site is worth more than the abstraction would save.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	// modernc.org/sqlite is a pure-Go SQLite. It is slower than the cgo
	// binding, which does not matter at this scale, and it means the whole app
	// cross-compiles to a static binary with CGO_ENABLED=0 — which is what
	// lets the container image be distroless with no libc.
	//
	// Go note: the blank import runs the package's init(), which registers the
	// "sqlite" driver name with database/sql. Nothing from it is called
	// directly, so without the blank identifier the compiler would reject the
	// unused import.
	_ "modernc.org/sqlite"
)

// migrationFiles holds the schema, compiled into the binary.
//
// Go note: //go:embed is a compiler directive, not a comment — it must sit
// immediately above the variable it fills, with no blank line between.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// timeFormat is how timestamps are stored. RFC3339 sorts lexicographically in
// the same order it sorts chronologically, so string comparison in SQL is also
// time comparison.
const timeFormat = time.RFC3339

// dateFormat is how due dates are stored. Incremental reading schedules in
// whole days, so due dates are dates.
const dateFormat = "2006-01-02"

// Store is a handle on the database.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, applies any pending migrations, and returns
// a ready Store. The file is created if it does not exist.
func Open(path string) (*Store, error) {
	// The pragmas matter more than they look:
	//   _pragma=journal_mode(WAL)  readers do not block the writer, so a sync
	//                              running in the background cannot stall the UI
	//   _pragma=busy_timeout(5000) wait for a lock instead of failing instantly
	//   _pragma=foreign_keys(ON)   SQLite disables FK enforcement by default,
	//                              which would make every ON DELETE CASCADE in
	//                              the schema a no-op
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	// SQLite tolerates exactly one writer. Capping the pool at one connection
	// turns "database is locked" errors into ordinary waiting.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect to %s: %w", path, err)
	}

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying handle for the rare caller that needs it, such as
// a health check.
func (s *Store) DB() *sql.DB {
	return s.db
}

// migrate applies every migration file not yet applied.
//
// Versioning uses SQLite's built-in user_version pragma rather than a
// migrations table: it is a single integer stored in the database header, it
// needs no bootstrapping, and for a single-writer application it is enough.
func (s *Store) migrate() error {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	// Filenames are zero-padded (001_, 002_), so lexical order is numeric order.
	sort.Strings(names)

	var current int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	for index, name := range names {
		version := index + 1
		if version <= current {
			continue
		}

		statements, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}

		transaction, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin migration %s: %w", name, err)
		}
		if _, err := transaction.Exec(string(statements)); err != nil {
			transaction.Rollback()
			return fmt.Errorf("store: apply migration %s: %w", name, err)
		}
		// PRAGMA does not accept a bound parameter, so the version is
		// interpolated. It is a loop counter, not user input.
		if _, err := transaction.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			transaction.Rollback()
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
		if err := transaction.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %s: %w", name, err)
		}
	}
	return nil
}

// formatTime renders a timestamp for storage, mapping the zero time to NULL.
//
// Go note: the return type is `any` because that is what database/sql accepts
// for a bound parameter, and it is the only way to pass a real SQL NULL.
func formatTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeFormat)
}

// parseTime reads a timestamp back, mapping NULL and unparseable values to the
// zero time.
func parseTime(value sql.NullString) time.Time {
	if !value.Valid || value.String == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(timeFormat, value.String)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

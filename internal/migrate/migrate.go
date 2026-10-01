// Package migrate applies the embedded SQL migrations at startup.
//
// Migrations are forward-only and recorded in a schema_migrations table, so
// running the server repeatedly is a no-op. Each file runs in its own
// transaction: a failure leaves the database on the last good version.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gorm.io/gorm"
)

const bookkeepingTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    TEXT PRIMARY KEY,
	applied_at DATETIME NOT NULL
)`

// Run applies every migration in files that has not been recorded yet.
func Run(ctx context.Context, db *gorm.DB, files fs.FS) error {
	if err := db.WithContext(ctx).Exec(bookkeepingTable).Error; err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	names, err := migrationFiles(files)
	if err != nil {
		return err
	}

	for _, name := range names {
		if _, ok := applied[name]; ok {
			continue
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		if err := apply(ctx, db, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func apply(ctx context.Context, db *gorm.DB, name, body string) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(body).Error; err != nil {
			return fmt.Errorf("migrate: apply %s: %w", name, err)
		}
		if err := tx.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, CURRENT_TIMESTAMP)", name).Error; err != nil {
			return fmt.Errorf("migrate: record %s: %w", name, err)
		}
		return nil
	})
}

func appliedVersions(ctx context.Context, db *gorm.DB) (map[string]struct{}, error) {
	var versions []string
	if err := db.WithContext(ctx).Raw("SELECT version FROM schema_migrations").Scan(&versions).Error; err != nil {
		return nil, fmt.Errorf("migrate: read applied versions: %w", err)
	}
	applied := make(map[string]struct{}, len(versions))
	for _, v := range versions {
		applied[v] = struct{}{}
	}
	return applied, nil
}

// migrationFiles lists *.sql files in lexical order, which is the ordering
// convention enforced by the zero-padded filename prefix.
func migrationFiles(files fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

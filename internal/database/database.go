// Package database opens and configures the SQLite connection used by GORM.
package database

import (
	"context"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"path/filepath"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"coverletter/internal/config"
)

// Open creates the parent directory if needed and opens the SQLite database.
//
// SQLite tolerates exactly one writer. The pool is therefore capped at
// MaxOpenConns (1 by default) so writes serialise in the pool rather than
// surfacing as SQLITE_BUSY under concurrency.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*gorm.DB, error) {
	if dir := filepath.Dir(cfg.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("database: create directory %q: %w", dir, err)
		}
	}

	// Busy timeout + foreign keys are connection-level pragmas; apply them via
	// the DSN so every pooled connection gets them.
	dsn := cfg.Path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

	// GORM's logger writes to a discard writer: the application's own slog
	// logger owns all output, and GORM errors surface through returned errors.
	gormLogger := logger.New(
		stdlog.New(io.Discard, "", 0),
		logger.Config{
			SlowThreshold:             500 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		},
	)

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:                                   gormLogger,
		DisableForeignKeyConstraintWhenMigrating: true,
		NowFunc:                                  func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("database: open %q: %w", cfg.Path, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("database: access sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return db, nil
}

// Close releases the underlying connection pool.
func Close(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

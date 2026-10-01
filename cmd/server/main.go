// Command server runs the cover letter generator backend.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"coverletter/internal/app"
	"coverletter/internal/config"
)

func main() {
	if err := run(); err != nil {
		// The logger may not be configured yet, so report to stderr directly
		// rather than dropping the only record of why the process failed.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		slog.Error("server exited with an error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	// SIGINT and SIGTERM cancel the context, which unwinds the server and the
	// worker pool through the graceful shutdown path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("initialise application: %w", err)
	}

	return application.Run(ctx)
}

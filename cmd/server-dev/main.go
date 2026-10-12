// Dev entry point that uses a scripted AI provider for local testing.
// No API key required — responses are deterministic and instant.
//
// Run with: make run-dev
// Or:       go run ./cmd/server-dev
package main

import (
	"context"
	"log"
	"os"

	"coverletter/internal/aitest"
	"coverletter/internal/app"
	"coverletter/internal/config"
)

func main() {
	// Load config from environment (.env file optional)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Use scripted fake provider — no network calls, no API key needed
	provider := aitest.DefaultResponses()

	// Build the app with the fake provider
	a, err := app.New(context.Background(), cfg, app.WithProvider(provider))
	if err != nil {
		log.Fatalf("app: %v", err)
	}

	// Determine port from env or default
	addr := cfg.HTTP.Addr
	if addr == "" {
		addr = ":8080"
	}

	log.Printf("starting dev server on %s (fake AI provider)", addr)

	// Run blocks until context cancelled
	ctx := context.Background()
	if err := a.Run(ctx); err != nil {
		log.Fatalf("run: %v", err)
	}

	// Graceful shutdown
	_ = os.Stdout.Sync()
}
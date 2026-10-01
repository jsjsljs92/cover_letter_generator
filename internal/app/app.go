// Package app wires the application together.
//
// Composition happens in exactly one place: here. Every other package receives
// its collaborators as parameters, which is what makes the whole system
// testable with fakes and keeps the dependency graph acyclic and visible.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"coverletter/internal/ai"
	"coverletter/internal/config"
	"coverletter/internal/coverletter"
	"coverletter/internal/database"
	"coverletter/internal/httpapi"
	"coverletter/internal/job"
	"coverletter/internal/logging"
	"coverletter/internal/migrate"
	"coverletter/internal/obs"
	"coverletter/internal/pdf"
	"coverletter/internal/resume"
	"coverletter/internal/storage"
	"coverletter/internal/worker"

	"coverletter/migrations"

	"gorm.io/gorm"
)

// maxResumeTextRunes bounds the resume text stored on the record and sent to
// the model. A longer resume adds cost without improving the letter.
const maxResumeTextRunes = 12000

// App holds the constructed application.
type App struct {
	cfg      config.Config
	logger   *slog.Logger
	db       *gorm.DB
	registry *obs.Registry
	pool     *worker.Pool
	service  *coverletter.Service
	handler  http.Handler
	server   *http.Server
}

// Option overrides a collaborator during construction. Tests use these to
// substitute a fake AI provider, job fetcher or PDF generator.
type Option func(*options)

type options struct {
	provider     ai.Provider
	jobFetcher   job.Fetcher
	jobParser    job.Parser
	resumeParser resume.Parser
	pdfGenerator pdf.Generator
}

// WithProvider replaces the AI provider.
func WithProvider(p ai.Provider) Option {
	return func(o *options) { o.provider = p }
}

// WithJobFetcher replaces the HTTP job fetcher, for example with a browser-backed
// implementation or a test fake.
func WithJobFetcher(f job.Fetcher) Option {
	return func(o *options) { o.jobFetcher = f }
}

// WithJobParser replaces the HTML parser.
func WithJobParser(p job.Parser) Option {
	return func(o *options) { o.jobParser = p }
}

// WithResumeParser replaces the PDF resume parser.
func WithResumeParser(p resume.Parser) Option {
	return func(o *options) { o.resumeParser = p }
}

// WithPDFGenerator replaces the PDF renderer.
func WithPDFGenerator(g pdf.Generator) Option {
	return func(o *options) { o.pdfGenerator = g }
}

// New builds the application: logger, metrics, database, migrations, storage,
// domain services, worker pool and HTTP router.
func New(ctx context.Context, cfg config.Config, opts ...Option) (*App, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	logger := logging.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(logger)
	registry := obs.New()

	db, err := database.Open(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	if err := migrate.Run(ctx, db, migrations.FS); err != nil {
		_ = database.Close(db)
		return nil, err
	}

	uploadStore, err := storage.NewLocal(cfg.Storage.UploadDir)
	if err != nil {
		_ = database.Close(db)
		return nil, err
	}
	pdfStore, err := storage.NewLocal(cfg.Storage.PDFDir)
	if err != nil {
		_ = database.Close(db)
		return nil, err
	}

	provider := o.provider
	if provider == nil {
		provider, err = ai.NewGeminiProvider(ctx, ai.GeminiConfig{
			APIKey:          cfg.AI.APIKey,
			Model:           cfg.AI.Model,
			Timeout:         cfg.AI.Timeout,
			MaxRetries:      cfg.AI.MaxRetries,
			Temperature:     cfg.AI.Temperature,
			MaxOutputTokens: cfg.AI.MaxOutputTokens,
		}, registry, logger)
		if err != nil {
			_ = database.Close(db)
			return nil, err
		}
	}

	agent := ai.NewAgent(provider, ai.AgentConfig{
		MaxAttempts:    cfg.Generate.MaxAttempts,
		MaxResumeRunes: maxResumeTextRunes,
		MaxJobRunes:    cfg.JobFetch.MaxDescriptionRunes,
	}, registry, logger)

	jobParser := o.jobParser
	if jobParser == nil {
		jobParser = job.NewHTMLParser(cfg.JobFetch.MaxDescriptionRunes)
	}
	jobFetcher := o.jobFetcher
	if jobFetcher == nil {
		jobFetcher = job.NewHTTPFetcher(
			cfg.JobFetch.Timeout,
			cfg.JobFetch.MaxRedirects,
			cfg.JobFetch.MaxHTMLBytes,
			cfg.JobFetch.UserAgent,
			cfg.JobFetch.AllowPrivateNetworks,
			registry,
			logger,
		)
	}

	resumeParser := o.resumeParser
	if resumeParser == nil {
		resumeParser = resume.NewPDFParser(maxResumeTextRunes)
	}

	jobService := job.NewServiceWithGuard(jobFetcher, jobParser, registry, cfg.JobFetch.AllowPrivateNetworks)
	resumeService := resume.NewService(uploadStore, uploadStore, resumeParser, cfg.HTTP.MaxUploadBytes, registry, logger)
	pdfGenerator := o.pdfGenerator
	if pdfGenerator == nil {
		pdfGenerator = pdf.NewGenerator()
	}

	pool := worker.New(cfg.Worker.Count, cfg.Worker.QueueSize, logger, registry)
	service := coverletter.NewService(
		coverletter.NewRepository(db),
		resumeService,
		jobService,
		agent,
		pdfGenerator,
		pdfStore,
		coverletter.Config{GenerationTimeout: cfg.Generate.Timeout},
		registry,
		logger,
	)

	dispatcher := &poolDispatcher{pool: pool, service: service}
	handler := coverletter.NewHandler(service, resumeService, dispatcher, cfg.HTTP.MaxUploadBytes, logger)
	router := httpapi.NewRouter(httpapi.Dependencies{
		CoverLetters: handler,
		Metrics:      registry,
		CORS: httpapi.CORSConfig{
			AllowedOrigins:   cfg.HTTP.CORSAllowedOrigins,
			AllowCredentials: cfg.HTTP.CORSAllowCredentials,
			MaxAge:           cfg.HTTP.CORSMaxAgeSeconds,
		},
	})

	return &App{
		cfg:      cfg,
		logger:   logger,
		db:       db,
		registry: registry,
		pool:     pool,
		service:  service,
		handler:  router,
		server: &http.Server{
			Addr:              cfg.HTTP.Addr,
			Handler:           router,
			ReadTimeout:       cfg.HTTP.ReadTimeout,
			ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
			WriteTimeout:      cfg.HTTP.WriteTimeout,
			IdleTimeout:       cfg.HTTP.IdleTimeout,
		},
	}, nil
}

// Handler returns the HTTP handler, for tests that drive the router directly.
func (a *App) Handler() http.Handler { return a.handler }

// Service returns the cover letter service, for tests that drive the workflow
// in-process without going through HTTP.
func (a *App) Service() *coverletter.Service { return a.service }

// Run starts the worker pool and HTTP server and blocks until ctx is cancelled,
// then shuts both down gracefully.
func (a *App) Run(ctx context.Context) error {
	a.pool.Start(ctx)

	serverErr := make(chan error, 1)
	go func() {
		a.logger.Info("http server listening",
			slog.String("addr", a.cfg.HTTP.Addr),
			slog.String("environment", a.cfg.Environment),
		)
		if err := a.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- fmt.Errorf("app: http server: %w", err)
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		// The server stopped on its own; stop the workers and clean up.
		a.shutdown()
		return err
	case <-ctx.Done():
		a.logger.Info("shutdown signal received")
	}

	// Stop accepting new requests first, then let in-flight work finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.HTTP.ShutdownTimeout)
	defer cancel()

	shutdownErr := a.server.Shutdown(shutdownCtx)
	a.shutdown()

	if shutdownErr != nil {
		return fmt.Errorf("app: graceful shutdown: %w", shutdownErr)
	}
	<-serverErr
	return nil
}

// shutdown stops the worker pool and closes the database.
func (a *App) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := a.pool.Shutdown(ctx); err != nil {
		a.logger.Error("worker pool shutdown failed", slog.String("error", err.Error()))
	}
	if err := database.Close(a.db); err != nil {
		a.logger.Error("database close failed", slog.String("error", err.Error()))
	}
}

// poolDispatcher adapts the worker pool to the cover letter service's Dispatcher
// port. It submits immediately rather than blocking, so a saturated queue
// surfaces to the client as backpressure instead of a hanging request.
type poolDispatcher struct {
	pool    *worker.Pool
	service *coverletter.Service
}

// Dispatch queues a generation job.
func (d *poolDispatcher) Dispatch(ctx context.Context, id uint) error {
	return d.pool.TrySubmit(ctx, worker.Task{
		Name: fmt.Sprintf("cover_letter:%d", id),
		Run: func(ctx context.Context) error {
			return d.service.Process(ctx, id)
		},
	})
}

// Ensure the dispatcher satisfies the port at compile time.
var _ coverletter.Dispatcher = (*poolDispatcher)(nil)

// unusedTimeGuard keeps the time import meaningful if the file is trimmed.
var _ = time.Second

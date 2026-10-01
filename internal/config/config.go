// Package config loads and validates the application configuration.
//
// Configuration comes exclusively from the environment; nothing is hard-coded in
// the wiring layer. Load fails fast on invalid input so the process does not
// start in a half-working state.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved application configuration.
type Config struct {
	Environment string

	HTTP     HTTPConfig
	Database DatabaseConfig
	Storage  StorageConfig
	AI       AIConfig
	Worker   WorkerConfig
	JobFetch JobFetchConfig
	Log      LogConfig
	Generate GenerationConfig
}

// HTTPConfig controls the HTTP server.
type HTTPConfig struct {
	Addr              string
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	// MaxUploadBytes caps the size of an individual multipart upload.
	MaxUploadBytes int64
	// CORSAllowedOrigins lists the browser origins permitted to call this API.
	// A single "*" allows any origin.
	CORSAllowedOrigins []string
	// CORSAllowCredentials permits cookies and Authorization headers.
	CORSAllowCredentials bool
	// CORSMaxAgeSeconds is how long a browser may cache a preflight result.
	CORSMaxAgeSeconds int
}

// DatabaseConfig controls the SQLite connection.
type DatabaseConfig struct {
	Path            string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// StorageConfig controls the local filesystem layout.
type StorageConfig struct {
	UploadDir string
	PDFDir    string
}

// AIConfig controls the model provider.
type AIConfig struct {
	Provider        string
	APIKey          string
	Model           string
	Timeout         time.Duration
	MaxRetries      int
	Temperature     float32
	MaxOutputTokens int
}

// WorkerConfig controls the background worker pool.
type WorkerConfig struct {
	Count     int
	QueueSize int
}

// JobFetchConfig controls the outbound HTTP fetcher used for job URLs.
type JobFetchConfig struct {
	Timeout              time.Duration
	MaxRedirects         int
	MaxHTMLBytes         int64
	UserAgent            string
	AllowPrivateNetworks bool
	MaxDescriptionRunes  int
}

// GenerationConfig controls the end-to-end cover letter workflow.
type GenerationConfig struct {
	// Timeout bounds a single background job, from extraction to PDF.
	Timeout time.Duration
	// MaxAttempts caps generation/validation retries in the AI agent.
	MaxAttempts int
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string
	Format string
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	cfg := Config{
		Environment: envString("APP_ENV", "development"),
		HTTP: HTTPConfig{
			Addr:              envString("HTTP_ADDR", ":8080"),
			ReadTimeout:       envDuration("HTTP_READ_TIMEOUT", 15*time.Second),
			ReadHeaderTimeout: envDuration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
			WriteTimeout:      envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:       envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout:   envDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
			MaxUploadBytes:    envInt64("MAX_UPLOAD_SIZE", 10<<20), // 10 MiB
			// Defaults to the Vite dev server, which serves the frontend
			// separately from this API and therefore needs an explicit grant.
			CORSAllowedOrigins:   envList("CORS_ALLOWED_ORIGINS", []string{"http://localhost:5173", "http://127.0.0.1:5173"}),
			CORSAllowCredentials: envBool("CORS_ALLOW_CREDENTIALS", false),
			CORSMaxAgeSeconds:    envInt("CORS_MAX_AGE_SECONDS", 600),
		},
		Database: DatabaseConfig{
			Path:            envString("DATABASE_PATH", "./data/app.db"),
			MaxOpenConns:    envInt("DATABASE_MAX_OPEN_CONNS", 1),
			MaxIdleConns:    envInt("DATABASE_MAX_IDLE_CONNS", 1),
			ConnMaxLifetime: envDuration("DATABASE_CONN_MAX_LIFETIME", time.Hour),
		},
		Storage: StorageConfig{
			UploadDir: envString("UPLOAD_DIR", "./data/uploads"),
			PDFDir:    envString("PDF_DIR", "./data/pdfs"),
		},
		AI: AIConfig{
			Provider:        envString("AI_PROVIDER", "gemini"),
			APIKey:          envString("GEMINI_API_KEY", ""),
			Model:           envString("GEMINI_MODEL", "gemini-2.5-flash"),
			Timeout:         envDuration("AI_TIMEOUT", 60*time.Second),
			MaxRetries:      envInt("AI_MAX_RETRIES", 2),
			Temperature:     float32(envFloat("AI_TEMPERATURE", 0.4)),
			MaxOutputTokens: envInt("AI_MAX_OUTPUT_TOKENS", 8192),
		},
		Worker: WorkerConfig{
			Count:     envInt("WORKER_COUNT", 2),
			QueueSize: envInt("WORKER_QUEUE_SIZE", 32),
		},
		JobFetch: JobFetchConfig{
			Timeout:              envDuration("JOB_FETCH_TIMEOUT", 15*time.Second),
			MaxRedirects:         envInt("JOB_FETCH_MAX_REDIRECTS", 5),
			MaxHTMLBytes:         envInt64("JOB_FETCH_MAX_HTML_BYTES", 4<<20), // 4 MiB
			UserAgent:            envString("JOB_FETCH_USER_AGENT", "CoverLetterGenerator/1.0 (+local development)"),
			AllowPrivateNetworks: envBool("JOB_FETCH_ALLOW_PRIVATE_NETWORKS", false),
			MaxDescriptionRunes:  envInt("JOB_MAX_DESCRIPTION_RUNES", 20000),
		},
		Generate: GenerationConfig{
			Timeout:     envDuration("GENERATION_TIMEOUT", 5*time.Minute),
			MaxAttempts: envInt("GENERATION_MAX_ATTEMPTS", 2),
		},
		Log: LogConfig{
			Level:  envString("LOG_LEVEL", "info"),
			Format: envString("LOG_FORMAT", "text"),
		},
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.AI.Provider {
	case "gemini":
		if c.AI.APIKey == "" {
			return fmt.Errorf("config: GEMINI_API_KEY must be set when AI_PROVIDER=gemini")
		}
	default:
		return fmt.Errorf("config: unsupported AI_PROVIDER %q", c.AI.Provider)
	}

	if c.HTTP.MaxUploadBytes <= 0 {
		return fmt.Errorf("config: MAX_UPLOAD_SIZE must be positive")
	}
	// The CORS spec forbids pairing a wildcard origin with credentials, and
	// browsers ignore the response rather than reporting a useful error. Fail
	// at boot instead, where the message can be acted on.
	if c.HTTP.CORSAllowCredentials {
		for _, origin := range c.HTTP.CORSAllowedOrigins {
			if origin == "*" {
				return fmt.Errorf("config: CORS_ALLOW_CREDENTIALS cannot be combined with a wildcard CORS_ALLOWED_ORIGINS")
			}
		}
	}
	if c.JobFetch.MaxHTMLBytes <= 0 {
		return fmt.Errorf("config: JOB_FETCH_MAX_HTML_BYTES must be positive")
	}
	if c.Worker.Count <= 0 {
		return fmt.Errorf("config: WORKER_COUNT must be positive")
	}
	if c.Worker.QueueSize <= 0 {
		return fmt.Errorf("config: WORKER_QUEUE_SIZE must be positive")
	}
	if c.AI.MaxRetries < 0 {
		return fmt.Errorf("config: AI_MAX_RETRIES must not be negative")
	}
	if c.Generate.MaxAttempts <= 0 {
		return fmt.Errorf("config: GENERATION_MAX_ATTEMPTS must be positive")
	}
	if c.AI.Temperature < 0 || c.AI.Temperature > 2 {
		return fmt.Errorf("config: AI_TEMPERATURE must be between 0 and 2")
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("config: LOG_FORMAT must be \"text\" or \"json\"")
	}
	return nil
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envList parses a comma-separated environment variable, falling back when it
// is unset or contains only separators and whitespace.
func envList(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func envInt64(key string, fallback int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func envFloat(key string, fallback float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

func envBool(key string, fallback bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return b
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return d
}

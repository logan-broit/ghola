package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/envcfg"
)

// Config holds all application configuration.
type Config struct {
	Environment string
	Server      ServerConfig
	Database    DatabaseConfig
	Embedding   EmbeddingConfig
	Auth        AuthConfig
	CORSOrigins []string
	// MentatURL is the base URL of the mentat service (cold-start cluster
	// embeddings, predictive replay). Empty string disables mentat-backed
	// paths; PR1.7/PR1.8 wiring checks for this before constructing a
	// client.
	MentatURL string
	// MentatClusterInterval is how often the Stage C clustering scheduler
	// fires. Default 24h matches the design doc; dev runs can dial down
	// (e.g. 1m) to iterate quickly.
	MentatClusterInterval time.Duration
	// MentatClusterWorkspaces is the explicit list of workspace UUIDs to
	// cluster on each tick. Empty list = scheduler runs but does
	// nothing — clustering is opt-in per workspace.
	MentatClusterWorkspaces []string
	// ConsolidateLLM configures the optional OpenAI-compatible chat
	// client used by POST /v1/semantic/consolidate for per-cluster
	// labels + the workspace digest. Empty URL means
	// consolidation.NewLLMClient returns nil and the pipeline skips
	// label/digest (never fails) — mirrors cmd/worker's CONSOLIDATE_LLM_*
	// handling so the manual trigger and the nightly job behave alike.
	ConsolidateLLM ConsolidateLLMConfig
}

// ConsolidateLLMConfig holds the optional chat-completions client
// configuration for consolidation labels/digest.
type ConsolidateLLMConfig struct {
	URL    string
	Model  string
	APIKey string
}

// ServerConfig holds HTTP server configuration.
type ServerConfig struct {
	Host            string
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// DatabaseConfig holds PostgreSQL connection configuration.
type DatabaseConfig struct {
	Host            string
	Port            int
	Name            string
	User            string
	Password        string
	SSLMode         string
	MaxConns        int
	MinConns        int
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// DSN returns the PostgreSQL connection string.
func (c DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		c.User, c.Password, c.Host, c.Port, c.Name, c.SSLMode,
	)
}

// EmbeddingConfig holds embedding provider configuration.
type EmbeddingConfig struct {
	Provider    string // OpenAI-compatible provider (Together.ai, vLLM, OpenAI, etc.)
	URL         string
	Model       string
	APIKey      string
	Dimensions  int
	Concurrency int
	Timeout     time.Duration
}

// AuthConfig holds authentication configuration.
type AuthConfig struct {
	Provider     string // "default" or "jwt"
	DefaultUser  string // UUID for default user in single-user mode
	JWTIssuer    string
	JWTAudience  string
	JWKSURL      string
	JWKSCacheTTL time.Duration
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		Environment: envcfg.String("ENVIRONMENT", "local"),
		Server: ServerConfig{
			Host:            envcfg.String("SERVER_HOST", "0.0.0.0"),
			Port:            envcfg.Int("SERVER_PORT", 8080),
			ReadTimeout:     envcfg.Duration("SERVER_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:    envcfg.Duration("SERVER_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: envcfg.Duration("SERVER_SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		Database: DatabaseConfig{
			Host:            envcfg.String("DATABASE_HOST", "localhost"),
			Port:            envcfg.Int("DATABASE_PORT", 5432),
			Name:            envcfg.String("DATABASE_NAME", "memories"),
			User:            envcfg.String("DATABASE_USER", "memory_api"),
			Password:        envcfg.String("DATABASE_PASSWORD", ""),
			SSLMode:         envcfg.String("DATABASE_SSL_MODE", "prefer"),
			MaxConns:        envcfg.Int("DATABASE_MAX_CONNS", 25),
			MinConns:        envcfg.Int("DATABASE_MIN_CONNS", 5),
			MaxConnLifetime: envcfg.Duration("DATABASE_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: envcfg.Duration("DATABASE_MAX_CONN_IDLE_TIME", 30*time.Minute),
		},
		Embedding: EmbeddingConfig{
			Provider:    envcfg.String("EMBEDDING_PROVIDER", "openai"),
			URL:         envcfg.String("EMBEDDING_URL", "https://api.openai.com"),
			Model:       envcfg.String("EMBEDDING_MODEL", "text-embedding-3-small"),
			APIKey:      envcfg.String("EMBEDDING_API_KEY", ""),
			Dimensions:  envcfg.Int("EMBEDDING_DIMENSIONS", 768),
			Concurrency: envcfg.Int("EMBEDDING_CONCURRENCY", 4),
			Timeout:     envcfg.Duration("EMBEDDING_TIMEOUT", 30*time.Second),
		},
		Auth: AuthConfig{
			Provider:     envcfg.String("AUTH_PROVIDER", "default"),
			DefaultUser:  envcfg.String("AUTH_DEFAULT_USER", "00000000-0000-0000-0000-000000000000"),
			JWTIssuer:    envcfg.String("JWT_ISSUER", ""),
			JWTAudience:  envcfg.String("JWT_AUDIENCE", ""),
			JWKSURL:      envcfg.String("JWKS_URL", ""),
			JWKSCacheTTL: envcfg.Duration("JWKS_CACHE_TTL", 15*time.Minute),
		},
		CORSOrigins:             parseCORSOrigins(envcfg.String("CORS_ORIGINS", "")),
		MentatURL:               envcfg.String("MENTAT_URL", ""),
		MentatClusterInterval:   envcfg.Duration("MENTAT_CLUSTER_INTERVAL", 24*time.Hour),
		MentatClusterWorkspaces: parseCSV(envcfg.String("MENTAT_CLUSTER_WORKSPACES", "")),
		ConsolidateLLM: ConsolidateLLMConfig{
			URL:    envcfg.String("CONSOLIDATE_LLM_URL", ""),
			Model:  envcfg.String("CONSOLIDATE_LLM_MODEL", "local-model"),
			APIKey: envcfg.String("CONSOLIDATE_LLM_API_KEY", ""),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.Auth.Provider == "jwt" {
		if c.Auth.JWKSURL == "" {
			return fmt.Errorf("JWKS_URL is required when AUTH_PROVIDER is jwt")
		}
		if c.Auth.JWTIssuer == "" {
			return fmt.Errorf("JWT_ISSUER is required when AUTH_PROVIDER is jwt")
		}
	}

	if c.IsProduction() && c.Auth.Provider == "default" {
		slog.Warn("AUTH_PROVIDER is 'default' in production — all API requests will bypass authentication. Set AUTH_PROVIDER to 'jwt' or ensure MCP endpoints use API key auth.")
	}

	return nil
}

// IsProduction returns true if running in production environment.
func (c *Config) IsProduction() bool {
	return c.Environment == "production"
}

// IsDevelopment returns true if running in a local development environment.
// Unknown environment names are treated as production (safe by default).
func (c *Config) IsDevelopment() bool {
	env := strings.ToLower(c.Environment)
	return env == "local" || env == "development"
}

// parseCORSOrigins splits a comma-separated origin string into a slice,
// filtering out empty entries.
func parseCORSOrigins(raw string) []string {
	return parseCSV(raw)
}

// parseCSV splits a comma-separated string and trims/filters empties.
// Generic helper used by CORS origins, cluster workspace lists, etc.
func parseCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

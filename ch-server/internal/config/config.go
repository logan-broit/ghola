package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	Environment string
	Server      ServerConfig
	Database    DatabaseConfig
	Qdrant      QdrantConfig
	Embedding   EmbeddingConfig
	Auth        AuthConfig
	CORSOrigins []string
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

// QdrantConfig holds Qdrant vector database configuration.
type QdrantConfig struct {
	Host       string
	HTTPPort   int
	GRPCPort   int
	APIKey     string
	UseTLS     bool
	Collection string
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
		Environment: getEnv("ENVIRONMENT", "local"),
		Server: ServerConfig{
			Host:            getEnv("SERVER_HOST", "0.0.0.0"),
			Port:            getEnvInt("SERVER_PORT", 8080),
			ReadTimeout:     getEnvDuration("SERVER_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:    getEnvDuration("SERVER_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout: getEnvDuration("SERVER_SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		Database: DatabaseConfig{
			Host:            getEnv("DATABASE_HOST", "localhost"),
			Port:            getEnvInt("DATABASE_PORT", 5432),
			Name:            getEnv("DATABASE_NAME", "memories"),
			User:            getEnv("DATABASE_USER", "memory_api"),
			Password:        getEnv("DATABASE_PASSWORD", ""),
			SSLMode:         getEnv("DATABASE_SSL_MODE", "prefer"),
			MaxConns:        getEnvInt("DATABASE_MAX_CONNS", 25),
			MinConns:        getEnvInt("DATABASE_MIN_CONNS", 5),
			MaxConnLifetime: getEnvDuration("DATABASE_MAX_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: getEnvDuration("DATABASE_MAX_CONN_IDLE_TIME", 30*time.Minute),
		},
		Qdrant: QdrantConfig{
			Host:       getEnv("QDRANT_HOST", "localhost"),
			HTTPPort:   getEnvInt("QDRANT_HTTP_PORT", 6333),
			GRPCPort:   getEnvInt("QDRANT_GRPC_PORT", 6334),
			APIKey:     getEnv("QDRANT_API_KEY", ""),
			UseTLS:     getEnvBool("QDRANT_TLS", false),
			Collection: getEnv("QDRANT_COLLECTION", "memories"),
		},
		Embedding: EmbeddingConfig{
			Provider:    getEnv("EMBEDDING_PROVIDER", "openai"),
			URL:         getEnv("EMBEDDING_URL", "https://api.openai.com"),
			Model:       getEnv("EMBEDDING_MODEL", "text-embedding-3-small"),
			APIKey:      getEnv("EMBEDDING_API_KEY", ""),
			Dimensions:  getEnvInt("EMBEDDING_DIMENSIONS", 768),
			Concurrency: getEnvInt("EMBEDDING_CONCURRENCY", 4),
			Timeout:     getEnvDuration("EMBEDDING_TIMEOUT", 30*time.Second),
		},
		Auth: AuthConfig{
			Provider:     getEnv("AUTH_PROVIDER", "default"),
			DefaultUser:  getEnv("AUTH_DEFAULT_USER", "00000000-0000-0000-0000-000000000000"),
			JWTIssuer:    getEnv("JWT_ISSUER", ""),
			JWTAudience:  getEnv("JWT_AUDIENCE", ""),
			JWKSURL:      getEnv("JWKS_URL", ""),
			JWKSCacheTTL: getEnvDuration("JWKS_CACHE_TTL", 15*time.Minute),
		},
		CORSOrigins: parseCORSOrigins(getEnv("CORS_ORIGINS", "")),
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

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return strings.EqualFold(value, "true") || value == "1"
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}

// parseCORSOrigins splits a comma-separated origin string into a slice,
// filtering out empty entries.
func parseCORSOrigins(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	var origins []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			origins = append(origins, p)
		}
	}
	return origins
}

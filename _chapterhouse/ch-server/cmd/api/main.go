package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/config"
	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"
	"github.com/thinkwright/chapterhouse/ch-server/internal/handler"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mcp"
	"github.com/thinkwright/chapterhouse/ch-server/internal/middleware"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mneme"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	logger.Info("starting Chapterhouse API server",
		slog.String("environment", cfg.Environment),
		slog.String("host", cfg.Server.Host),
		slog.Int("port", cfg.Server.Port),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	poolConfig, err := pgxpool.ParseConfig(cfg.Database.DSN())
	if err != nil {
		return fmt.Errorf("failed to parse database config: %w", err)
	}

	poolConfig.MaxConns = int32(cfg.Database.MaxConns)
	poolConfig.MinConns = int32(cfg.Database.MinConns)
	poolConfig.MaxConnLifetime = cfg.Database.MaxConnLifetime
	poolConfig.MaxConnIdleTime = cfg.Database.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	logger.Info("connected to database",
		slog.String("host", cfg.Database.Host),
		slog.String("database", cfg.Database.Name),
	)

	repo := repository.New(pool)
	queries := repo.Queries()

	var authProvider auth.Provider
	switch cfg.Auth.Provider {
	case "jwt":
		authProvider = auth.NewJWTProvider(
			cfg.Auth.JWKSURL,
			cfg.Auth.JWTIssuer,
			cfg.Auth.JWTAudience,
			cfg.Auth.JWKSCacheTTL,
		)
		logger.Info("using JWT authentication")
	default:
		authProvider, err = auth.NewDefaultProvider(
			cfg.Auth.DefaultUser,
			"default",
			"user@localhost",
		)
		if err != nil {
			return fmt.Errorf("failed to create default auth provider: %w", err)
		}
		logger.Info("using default authentication",
			slog.String("default_user", cfg.Auth.DefaultUser),
		)
	}

	var embedder embedding.Provider
	embeddingCfg := embedding.Config{
		URL:         cfg.Embedding.URL,
		Model:       cfg.Embedding.Model,
		Dimensions:  cfg.Embedding.Dimensions,
		Concurrency: cfg.Embedding.Concurrency,
	}
	embedder = embedding.NewOpenAIProvider(embeddingCfg, cfg.Embedding.APIKey)
	logger.Info("using OpenAI-compatible embedding provider",
		slog.String("url", cfg.Embedding.URL),
		slog.String("model", cfg.Embedding.Model),
	)

	// pg_ghola store — replaces Qdrant + memory_blocks
	store := mneme.NewStore(pool, embedder, logger)

	healthHandler := handler.NewHealthHandler(pool)
	sessionProvider := auth.NewSessionProviderWithAdapter(auth.SessionProviderConfig{
		Queries:         queries,
		SessionDuration: 8 * time.Hour,
		SecureCookies:   !cfg.IsDevelopment(),
	})

	adminHandler := handler.NewAdminHandler(queries, sessionProvider)

	systemStatsHandler := handler.NewSystemStatsHandler(pool)

	apiKeyProvider := auth.NewAPIKeyProviderWithAdapter(queries)
	apiAuthProvider := auth.NewCompositeProvider(apiKeyProvider, authProvider)

	// MCP endpoints only accept API key auth (no fallback to default provider)
	mcpServer := mcp.NewServer(store, queries, logger)
	mcpHTTPHandler := mcp.NewStreamableHTTPHandler(mcpServer, apiKeyProvider, logger)
	mcpStatelessHandler := mcp.NewStatelessHTTPHandler(mcpServer, apiKeyProvider, logger)

	// Rate limiters: login (5 req/min per IP), MCP (10 req/sec, burst 50 per user)
	loginLimiter := middleware.NewRateLimiter(5.0/60.0, 5)
	defer loginLimiter.Close()
	mcpLimiter := middleware.NewRateLimiter(10, 50)
	defer mcpLimiter.Close()

	router := buildRouter(cfg, logger, apiAuthProvider, sessionProvider, healthHandler, adminHandler, systemStatsHandler, mcpHTTPHandler, mcpStatelessHandler, loginLimiter, mcpLimiter)

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("HTTP server listening",
			slog.String("addr", server.Addr),
		)
		healthHandler.SetReady(true)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return fmt.Errorf("server error: %w", err)
	case sig := <-shutdown:
		logger.Info("shutdown signal received", slog.String("signal", sig.String()))
	}

	healthHandler.SetReady(false)

	ctx, cancel = context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("error during shutdown", slog.String("error", err.Error()))
		return server.Close()
	}

	// Stop session cleanup goroutine and wait for in-flight background operations
	mcpHTTPHandler.Close()
	mcpServer.Wait()

	logger.Info("server shutdown complete")
	return nil
}

func buildRouter(
	cfg *config.Config,
	logger *slog.Logger,
	authProvider auth.Provider,
	sessionProvider *auth.SessionProvider,
	healthHandler *handler.HealthHandler,
	adminHandler *handler.AdminHandler,
	systemStatsHandler *handler.SystemStatsHandler,
	mcpHTTPHandler *mcp.StreamableHTTPHandler,
	mcpStatelessHandler *mcp.StatelessHTTPHandler,
	loginLimiter *middleware.RateLimiter,
	mcpLimiter *middleware.RateLimiter,
) *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.RequestIDMiddleware)
	r.Use(middleware.RecoveryMiddleware(logger))
	r.Use(middleware.LoggingMiddleware(logger))

	if len(cfg.CORSOrigins) > 0 {
		r.Use(middleware.CORSMiddleware(cfg.CORSOrigins))
	}

	// Health endpoints (no auth required)
	r.Get("/health", healthHandler.Health)
	r.Get("/ready", healthHandler.Ready)

	// MCP endpoints - both transports available
	// /mcp - Streamable HTTP transport (session-based, 2025-06-18 spec)
	// /mcp/stateless - Simple stateless HTTP (for Claude Code `-t http`)
	r.Group(func(r chi.Router) {
		r.Use(middleware.IPRateLimitMiddleware(mcpLimiter))
		r.Handle("/mcp", mcpHTTPHandler)
		r.Handle("/mcp/stateless", mcpStatelessHandler)
	})

	// API routes
	r.Route("/api/v1", func(r chi.Router) {
		// User self-service routes (session auth only - for web UI)
		r.Route("/user", func(r chi.Router) {
			r.Use(middleware.ContentTypeJSON)
			r.Use(middleware.AuthMiddleware(sessionProvider))

			r.Get("/audit", adminHandler.ListUserAuditLogs)

			r.Get("/keys", adminHandler.ListUserAPIKeys)
			r.Post("/keys", adminHandler.CreateUserAPIKey)
			r.Delete("/keys/{id}", adminHandler.RevokeUserAPIKey)

			r.Post("/password", adminHandler.ChangePassword)
		})

		// Admin routes (session auth only - for web UI)
		r.Route("/admin", func(r chi.Router) {
			r.Use(middleware.ContentTypeJSON)

			// Login endpoint (no auth required, IP rate-limited)
			r.With(middleware.IPRateLimitMiddleware(loginLimiter)).Post("/login", adminHandler.Login)

			// Authenticated routes (any logged-in user)
			r.Group(func(r chi.Router) {
				r.Use(middleware.AuthMiddleware(sessionProvider))

				r.Post("/logout", adminHandler.Logout)
				r.Get("/me", adminHandler.GetCurrentUser)
			})

			// Admin-only routes (session auth + admin role required)
			r.Group(func(r chi.Router) {
				r.Use(middleware.AuthMiddleware(sessionProvider))
				r.Use(middleware.RequireAdmin)

				r.Get("/stats", adminHandler.GetStats)
				r.Get("/system-stats", systemStatsHandler.GetSystemStats)
				r.Get("/memory-type-distribution", systemStatsHandler.GetMemoryTypeDistribution)
				r.Get("/memory-scope-distribution", systemStatsHandler.GetMemoryScopeDistribution)
				r.Get("/top-tags", systemStatsHandler.GetTopTags)
				r.Get("/audit", adminHandler.ListAuditLogs)

				r.Route("/users", func(r chi.Router) {
					r.Get("/", adminHandler.ListUsers)
					r.Post("/", adminHandler.CreateUser)
					r.Get("/{id}", adminHandler.GetUser)
					r.Put("/{id}", adminHandler.UpdateUser)
					r.Delete("/{id}", adminHandler.DeactivateUser)
					r.Post("/{id}/reactivate", adminHandler.ReactivateUser)
					r.Get("/{id}/keys", adminHandler.ListAPIKeys)
					r.Post("/{id}/keys", adminHandler.CreateAPIKey)
				})

				r.Get("/keys", adminHandler.ListAllAPIKeys)
				r.Delete("/keys/{id}", adminHandler.RevokeAPIKey)
			})
		})
	})

	return r
}

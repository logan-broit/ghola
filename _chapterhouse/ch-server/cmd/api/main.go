package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/config"
	"github.com/thinkwright/chapterhouse/ch-server/internal/handler"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/middleware"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/semantic"

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

	// Semantic L1 write path (PR1.7): when mentat is configured, run a
	// background reconciler that pools closed sessions' events into the
	// episodic.sessions.l1_embedding column. With MENTAT_URL unset we
	// skip the goroutine entirely — chapterhouse remains useful as a
	// pure REST surface even without the cold-start service running.
	//
	// The same `mentatClient` (nil-tolerant when MENTAT_URL is empty)
	// is also handed to the read-path Querier (PR1.8) below. A nil
	// client makes Querier.Recall return zero hits, preserving the
	// design invariant that semantic-tier failure never breaks recall.
	var mentatClient *mentat.Client
	cancelReconciler := func() {}
	if cfg.MentatURL != "" {
		mentatClient = mentat.NewClient(cfg.MentatURL, nil)
		writer := semantic.NewWriter(repo, mentatClient)
		reconciler := semantic.NewReconciler(writer, 30*time.Second, logger)

		var reconcilerCtx context.Context
		reconcilerCtx, cancelReconciler = context.WithCancel(context.Background())
		defer cancelReconciler()

		go func() {
			logger.Info("starting semantic L1 reconciler",
				slog.String("mentat_url", cfg.MentatURL),
			)
			if err := reconciler.Run(reconcilerCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("semantic reconciler exited",
					slog.String("error", err.Error()),
				)
			}
		}()
	} else {
		// Single-line operator hint: with MENTAT_URL unset the read path
		// (semantic.Querier) accepts a nil client and short-circuits to
		// zero hits. Log it once at boot so "why does recall return 0?"
		// has an obvious answer in the boot log instead of in trace logs.
		logger.Info("semantic recall disabled — MENTAT_URL unset; queries return 0 hits")
	}

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

	healthHandler := handler.NewHealthHandler(pool)
	sessionProvider := auth.NewSessionProviderWithAdapter(auth.SessionProviderConfig{
		Queries:         queries,
		SessionDuration: 8 * time.Hour,
		SecureCookies:   !cfg.IsDevelopment(),
	})

	adminHandler := handler.NewAdminHandler(queries, sessionProvider)

	apiKeyProvider := auth.NewAPIKeyProviderWithAdapter(queries)
	apiAuthProvider := auth.NewCompositeProvider(apiKeyProvider, authProvider)

	// v2 /v1 surface — consumed only by the ghola local service.
	episodicHandler := handler.NewEpisodicHandler(repo)
	querier := semantic.NewQuerier(repo, mentatClient, logger)
	semanticHandler := handler.NewSemanticHandler(querier)

	// Legacy MCP transport removed in v2: agents now talk to the
	// ghola local service, which in turn calls chapterhouse's
	// internal /v1 REST surface. The old MCP code is kept in
	// internal/mcp_legacy for reference.

	// Rate limiter for the admin login path only (MCP limiter dropped
	// with the MCP route).
	loginLimiter := middleware.NewRateLimiter(5.0/60.0, 5)
	defer loginLimiter.Close()

	router := buildRouter(
		cfg, logger,
		sessionProvider, apiAuthProvider,
		healthHandler, adminHandler,
		episodicHandler, semanticHandler,
		loginLimiter,
	)

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

	// Stop the semantic reconciler eagerly so it isn't still ticking
	// while the HTTP server drains in-flight requests below. The
	// deferred cancelReconciler() above remains as a safety net.
	cancelReconciler()

	healthHandler.SetReady(false)

	ctx, cancel = context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Error("error during shutdown", slog.String("error", err.Error()))
		return server.Close()
	}

	logger.Info("server shutdown complete")
	return nil
}

func buildRouter(
	cfg *config.Config,
	logger *slog.Logger,
	sessionProvider *auth.SessionProvider,
	apiAuthProvider auth.Provider,
	healthHandler *handler.HealthHandler,
	adminHandler *handler.AdminHandler,
	episodicHandler *handler.EpisodicHandler,
	semanticHandler *handler.SemanticHandler,
	loginLimiter *middleware.RateLimiter,
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

	// v2 internal API — called only by the ghola local service.
	// Bearer API key required; middleware populates auth.Context which
	// the handlers read via auth.UserIDFromContext.
	r.Route("/v1", func(r chi.Router) {
		r.Use(middleware.AuthMiddleware(apiAuthProvider))
		r.Use(middleware.ContentTypeJSON)

		r.Route("/episodic", func(r chi.Router) {
			r.Post("/ingest", episodicHandler.Ingest)
			r.Post("/query", episodicHandler.Query)
			r.Post("/share", episodicHandler.Share)
			r.Post("/forget", episodicHandler.Forget)
		})

		r.Route("/semantic", func(r chi.Router) {
			// v0.3 narrows the surface to /query only. The v0.2
			// /feedback and /list paths are dropped: feedback is
			// replaced by the dogfooding-tags mechanism in PR7, and
			// /list returned mnemes with concept/content fields that
			// no longer exist on the v0.3 schema.
			r.Post("/query", semanticHandler.Query)
		})
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

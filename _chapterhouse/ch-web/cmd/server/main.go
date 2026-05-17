package main

import (
	"embed"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

//go:embed all:static
var staticFiles embed.FS

// proxyClient is a shared HTTP client for proxying API requests.
var proxyClient = &http.Client{Timeout: 30 * time.Second}

var logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

func main() {
	port := flag.String("port", getEnv("PORT", "8080"), "Port to listen on")
	apiURL := flag.String("api-url", getEnv("API_URL", "http://ch-server:8080"), "Chapterhouse API server URL")
	flag.Parse()

	mux := http.NewServeMux()

	// Proxy API requests to the ch-server
	mux.HandleFunc("/api/", proxyHandler(*apiURL))

	// Health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Serve static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		logger.Error("static fs", slog.String("error", err.Error()))
		os.Exit(1)
	}
	mux.Handle("/", securityHeaders(spaHandler(http.FS(staticFS))))

	logger.Info("ch-web listening",
		slog.String("port", *port),
		slog.String("api_proxy_target", *apiURL),
	)
	if err := http.ListenAndServe(":"+*port, mux); err != nil {
		logger.Error("listen", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// getEnv reads key from the env, falling back to def when unset or empty.
// ch-web is a separate go module from ch-server so we don't share
// internal/envcfg here — single helper, two callers, kept inline.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// securityHeaders wraps a handler to set standard security headers on all responses.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}

// spaHandler serves static files with admin routing
func spaHandler(fsys http.FileSystem) http.Handler {
	fileServer := http.FileServer(fsys)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Root serves landing page
		if path == "/" {
			serveFile(w, r, fsys, "/index.html")
			return
		}

		// /admin without trailing slash redirects
		if path == "/admin" {
			http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
			return
		}

		// /admin/ serves login page
		if path == "/admin/" {
			serveFile(w, r, fsys, "/login.html")
			return
		}

		// /admin/* routes to admin pages
		if strings.HasPrefix(path, "/admin/") {
			adminPath := strings.TrimPrefix(path, "/admin")
			f, err := fsys.Open(adminPath)
			if err != nil {
				serveFile(w, r, fsys, "/login.html")
				return
			}
			f.Close()
			serveFile(w, r, fsys, adminPath)
			return
		}

		// Try to open the file directly
		f, err := fsys.Open(path)
		if err != nil {
			serveFile(w, r, fsys, "/login.html")
			return
		}
		f.Close()

		// Cache headers for static assets
		if strings.HasSuffix(path, ".css") || strings.HasSuffix(path, ".js") {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		} else if strings.HasSuffix(path, ".html") {
			w.Header().Set("Cache-Control", "no-cache")
		}

		fileServer.ServeHTTP(w, r)
	})
}

// serveFile serves a specific file from the filesystem
func serveFile(w http.ResponseWriter, r *http.Request, fsys http.FileSystem, name string) {
	f, err := fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if strings.HasSuffix(name, ".html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	} else if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
	} else if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	http.ServeContent(w, r, name, stat.ModTime(), f.(io.ReadSeeker))
}

// proxyHandler forwards requests to the API server.
// Uses a shared http.Client with timeout. Validates the constructed URL
// stays within the expected API host.
func proxyHandler(baseURL string) http.HandlerFunc {
	parsedBase, err := url.Parse(strings.TrimSuffix(baseURL, "/"))
	if err != nil {
		logger.Error("invalid API_URL", slog.String("error", err.Error()))
		os.Exit(1)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// Construct target URL and validate it stays on the expected host.
		target, err := url.Parse(parsedBase.String() + r.URL.Path)
		if err != nil || target.Host != parsedBase.Host {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		target.RawQuery = r.URL.RawQuery

		proxyReq, err := http.NewRequest(r.Method, target.String(), r.Body)
		if err != nil {
			http.Error(w, "Failed to create proxy request", http.StatusInternalServerError)
			return
		}

		// Forward safe headers only.
		for _, key := range []string{"Content-Type", "Accept", "Cookie", "Authorization"} {
			if v := r.Header.Get(key); v != "" {
				proxyReq.Header.Set(key, v)
			}
		}

		resp, err := proxyClient.Do(proxyReq)
		if err != nil {
			http.Error(w, "Failed to reach API server", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}

		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}
}

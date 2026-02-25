package main

import (
	"embed"
	"flag"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
)

//go:embed all:static
var staticFiles embed.FS

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
		log.Fatal(err)
	}
	mux.Handle("/", spaHandler(http.FS(staticFS)))

	log.Printf("Chapterhouse Admin listening on :%s", *port)
	log.Printf("API proxy target: %s", *apiURL)
	if err := http.ListenAndServe(":"+*port, mux); err != nil {
		log.Fatal(err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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

// proxyHandler forwards requests to the API server
func proxyHandler(baseURL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		targetURL := strings.TrimSuffix(baseURL, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		proxyReq, err := http.NewRequest(r.Method, targetURL, r.Body)
		if err != nil {
			http.Error(w, "Failed to create proxy request", http.StatusInternalServerError)
			return
		}

		for key, values := range r.Header {
			for _, value := range values {
				proxyReq.Header.Add(key, value)
			}
		}

		for _, cookie := range r.Cookies() {
			proxyReq.AddCookie(cookie)
		}

		client := &http.Client{}
		resp, err := client.Do(proxyReq)
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

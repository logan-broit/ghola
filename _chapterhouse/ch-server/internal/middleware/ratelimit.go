package middleware

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"
)

// bucket tracks token-bucket state for a single key.
type bucket struct {
	tokens    float64
	lastCheck time.Time
}

// RateLimiter is a simple in-memory token-bucket rate limiter.
// Keys are arbitrary strings (IP addresses, user IDs, etc.).
type RateLimiter struct {
	rate     float64 // tokens added per second
	burst    int     // max tokens (bucket size)
	mu       sync.Mutex
	buckets  map[string]*bucket
	done     chan struct{}
}

// NewRateLimiter creates a rate limiter that allows rate requests per second
// with a maximum burst size.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		rate:    rate,
		burst:   burst,
		buckets: make(map[string]*bucket),
		done:    make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

// Allow checks whether a request for the given key should be allowed.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, exists := rl.buckets[key]
	if !exists {
		rl.buckets[key] = &bucket{
			tokens:    float64(rl.burst) - 1,
			lastCheck: now,
		}
		return true
	}

	elapsed := now.Sub(b.lastCheck).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.lastCheck = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Close stops the background cleanup goroutine.
func (rl *RateLimiter) Close() {
	close(rl.done)
}

// cleanup removes stale entries every minute.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rl.done:
			return
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for key, b := range rl.buckets {
				if now.Sub(b.lastCheck) > 5*time.Minute {
					delete(rl.buckets, key)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// IPRateLimitMiddleware limits requests per client IP.
func IPRateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if !limiter.Allow(ip) {
				apierror.TooManyRequests("Rate limit exceeded. Try again later.").WriteJSON(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// UserRateLimitMiddleware limits requests per authenticated user.
// Must be placed AFTER AuthMiddleware in the chain.
func UserRateLimitMiddleware(limiter *RateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCtx := auth.FromContext(r.Context())
			if authCtx == nil {
				next.ServeHTTP(w, r)
				return
			}
			if !limiter.Allow(authCtx.UserID.String()) {
				apierror.TooManyRequests("Rate limit exceeded. Try again later.").WriteJSON(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP extracts the client IP, preferring X-Forwarded-For.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP (original client)
		if i := len(xff); i > 0 {
			for j := 0; j < len(xff); j++ {
				if xff[j] == ',' {
					return xff[:j]
				}
			}
			return xff
		}
	}
	if xff := r.Header.Get("X-Real-IP"); xff != "" {
		return xff
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

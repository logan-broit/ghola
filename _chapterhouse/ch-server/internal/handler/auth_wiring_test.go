package handler_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/middleware"
)

// fakeAuthProvider lets us drive the three auth-middleware cases
// deterministically without booting the full APIKeyProvider / user DB
// stack — that provider has its own tests in internal/auth.
type fakeAuthProvider struct {
	ok     bool
	userID uuid.UUID
}

func (p *fakeAuthProvider) Authenticate(r *http.Request) (*auth.Context, error) {
	hdr := r.Header.Get("Authorization")
	if hdr == "" {
		return nil, errors.New("missing Authorization header")
	}
	if !p.ok || !strings.HasPrefix(hdr, "Bearer valid-") {
		return nil, errors.New("invalid api key")
	}
	return &auth.Context{UserID: p.userID}, nil
}

// TestV1_AuthMiddleware_Wiring exercises the three auth cases the
// Phase 3.8 plan calls out (valid / invalid / missing) at the
// router+middleware level. The handler itself is a trivial sentinel
// that writes 200 when reached, so any test failure here is about
// the middleware wiring — not the handler's own auth check (which
// episodic_test.go / semantic_test.go cover separately).
func TestV1_AuthMiddleware_Wiring(t *testing.T) {
	userID := uuid.New()
	provider := &fakeAuthProvider{ok: true, userID: userID}

	var reached bool
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.UserIDFromContext(r.Context()) != userID {
			t.Errorf("handler reached without user id in ctx")
		}
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(middleware.AuthMiddleware(provider))
		r.Post("/episodic/ingest", sentinel)
	})

	cases := []struct {
		name       string
		authHeader string
		wantCode   int
		wantReach  bool
	}{
		{"valid", "Bearer valid-abc", http.StatusOK, true},
		{"invalid", "Bearer not-our-prefix", http.StatusUnauthorized, false},
		{"missing", "", http.StatusUnauthorized, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(http.MethodPost, "/v1/episodic/ingest", strings.NewReader(`{}`))
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, tc.wantCode, rec.Code, "status code")
			assert.Equal(t, tc.wantReach, reached, "handler reached?")
		})
	}
}

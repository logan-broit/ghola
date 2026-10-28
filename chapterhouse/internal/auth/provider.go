package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrUnauthorized    = errors.New("unauthorized")
	ErrInvalidToken    = errors.New("invalid token")
	ErrTokenExpired    = errors.New("token expired")
	ErrMissingToken    = errors.New("missing authorization token")
	ErrInvalidAudience = errors.New("invalid token audience")
	ErrInvalidIssuer   = errors.New("invalid token issuer")
)

// Provider defines the interface for authentication providers.
type Provider interface {
	// Authenticate extracts and validates authentication from the request.
	Authenticate(r *http.Request) (*Context, error)
}

// DefaultProvider provides a static user context for single-user development.
type DefaultProvider struct {
	defaultUserID uuid.UUID
	username      string
	email         string
}

// NewDefaultProvider creates a new default authentication provider.
func NewDefaultProvider(defaultUserID, username, email string) (*DefaultProvider, error) {
	uid, err := uuid.Parse(defaultUserID)
	if err != nil {
		return nil, fmt.Errorf("invalid default user ID: %w", err)
	}
	return &DefaultProvider{
		defaultUserID: uid,
		username:      username,
		email:         email,
	}, nil
}

// Authenticate always returns the default user context.
func (p *DefaultProvider) Authenticate(_ *http.Request) (*Context, error) {
	return &Context{
		UserID:   p.defaultUserID,
		Username: p.username,
		Email:    p.email,
		Roles:    []string{"user"},
	}, nil
}

// JWTProvider validates JWT tokens against a JWKS endpoint.
type JWTProvider struct {
	jwksURL  string
	issuer   string
	audience string
	cacheTTL time.Duration
	client   *http.Client

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	cacheTime time.Time
}

// NewJWTProvider creates a new JWT authentication provider.
func NewJWTProvider(jwksURL, issuer, audience string, cacheTTL time.Duration) *JWTProvider {
	return &JWTProvider{
		jwksURL:  jwksURL,
		issuer:   issuer,
		audience: audience,
		cacheTTL: cacheTTL,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		keys: make(map[string]*rsa.PublicKey),
	}
}

// Authenticate validates the JWT bearer token from the Authorization header.
func (p *JWTProvider) Authenticate(r *http.Request) (*Context, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, ErrMissingToken
	}

	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, ErrInvalidToken
	}

	tokenString := strings.TrimPrefix(authHeader, "Bearer ")

	token, err := jwt.Parse(tokenString, p.keyFunc,
		jwt.WithIssuer(p.issuer),
		jwt.WithAudience(p.audience),
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512"}),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	return p.extractContext(claims)
}

func (p *JWTProvider) keyFunc(token *jwt.Token) (any, error) {
	kid, ok := token.Header["kid"].(string)
	if !ok {
		return nil, fmt.Errorf("missing kid in token header")
	}

	key, err := p.getKey(kid)
	if err != nil {
		return nil, err
	}

	return key, nil
}

func (p *JWTProvider) getKey(kid string) (*rsa.PublicKey, error) {
	p.mu.RLock()
	if key, ok := p.keys[kid]; ok && time.Since(p.cacheTime) < p.cacheTTL {
		p.mu.RUnlock()
		return key, nil
	}
	p.mu.RUnlock()

	return p.refreshKeys(kid)
}

func (p *JWTProvider) refreshKeys(kid string) (*rsa.PublicKey, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock
	if key, ok := p.keys[kid]; ok && time.Since(p.cacheTime) < p.cacheTTL {
		return key, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create JWKS request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned status %d", resp.StatusCode)
	}

	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}

	// Clear old keys and populate new ones
	p.keys = make(map[string]*rsa.PublicKey)
	p.cacheTime = time.Now()

	for _, key := range jwks.Keys {
		if key.Kty != "RSA" || key.Use != "sig" {
			continue
		}

		pubKey, err := parseRSAPublicKey(key.N, key.E)
		if err != nil {
			continue
		}

		p.keys[key.Kid] = pubKey
	}

	if key, ok := p.keys[kid]; ok {
		return key, nil
	}

	return nil, fmt.Errorf("key with kid %s not found in JWKS", kid)
}

func (p *JWTProvider) extractContext(claims jwt.MapClaims) (*Context, error) {
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, fmt.Errorf("missing sub claim")
	}

	userID, err := uuid.Parse(sub)
	if err != nil {
		return nil, fmt.Errorf("invalid sub claim: %w", err)
	}

	username, _ := claims["preferred_username"].(string)
	email, _ := claims["email"].(string)

	var roles []string
	if realmAccess, ok := claims["realm_access"].(map[string]any); ok {
		if rolesRaw, ok := realmAccess["roles"].([]any); ok {
			for _, r := range rolesRaw {
				if role, ok := r.(string); ok {
					roles = append(roles, role)
				}
			}
		}
	}

	// Convert claims to map[string]any
	claimsMap := make(map[string]any)
	for k, v := range claims {
		claimsMap[k] = v
	}

	return &Context{
		UserID:   userID,
		Username: username,
		Email:    email,
		Roles:    roles,
		Claims:   claimsMap,
	}, nil
}

// parseRSAPublicKey parses RSA public key from JWK n and e values (base64url-encoded).
func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}

	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}

	// Convert exponent bytes to int
	var exp int
	for _, b := range eBytes {
		exp = exp<<8 + int(b)
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: exp,
	}, nil
}

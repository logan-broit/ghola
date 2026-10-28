package auth

import (
	"errors"
	"net/http"
)

// CompositeProvider tries multiple authentication providers in order.
// It returns the first successful authentication result, or the last error
// if all providers fail.
type CompositeProvider struct {
	providers []Provider
}

// NewCompositeProvider creates a new composite provider that tries providers in order.
func NewCompositeProvider(providers ...Provider) *CompositeProvider {
	return &CompositeProvider{
		providers: providers,
	}
}

// Authenticate tries each provider in order until one succeeds.
// If a provider returns ErrMissingToken, it tries the next provider.
// If a provider returns any other error (like invalid token), it still tries
// the next provider, but tracks the error to return if all fail.
// Returns the first successful result, or an error if all providers fail.
func (p *CompositeProvider) Authenticate(r *http.Request) (*Context, error) {
	if len(p.providers) == 0 {
		return nil, ErrMissingToken
	}

	var lastError error
	for _, provider := range p.providers {
		ctx, err := provider.Authenticate(r)
		if err == nil {
			return ctx, nil
		}

		// Track non-missing-token errors as they're more informative
		if !errors.Is(err, ErrMissingToken) {
			lastError = err
		} else if lastError == nil {
			lastError = err
		}
	}

	return nil, lastError
}

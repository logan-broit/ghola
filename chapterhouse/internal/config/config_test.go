package config

import "testing"

func TestAuthDisabledRefusedInProduction(t *testing.T) {
	t.Setenv("AUTH_DISABLED", "true")
	t.Setenv("ENVIRONMENT", "production")
	if _, err := Load(); err == nil {
		t.Fatal("Load must fail with AUTH_DISABLED=true in production")
	}
	t.Setenv("ENVIRONMENT", "local")
	if _, err := Load(); err != nil {
		t.Fatalf("Load in local dev: %v", err)
	}
}

package config

import (
	"strings"
	"testing"
)

func TestJWTSecretInsecure(t *testing.T) {
	cases := []struct {
		secret string
		want   bool
	}{
		{"", true},
		{DevJWTSecret, true},
		{"CHANGE-ME-TO-A-RANDOM-HEX-STRING", true},
		{"change-me", true},
		{"secret", true},
		{"a-sufficiently-long-random-value-32", false},
	}
	for _, tc := range cases {
		cfg := &Config{JWTSecret: tc.secret}
		if got := cfg.JWTSecretInsecure(); got != tc.want {
			t.Errorf("JWTSecret=%q insecure=%v want %v", tc.secret, got, tc.want)
		}
	}
}

func TestValidateRefusesInsecureSecretInProduction(t *testing.T) {
	t.Setenv("GSC_ENV", "production")
	t.Setenv("GIN_MODE", "")
	cfg := &Config{JWTSecret: DevJWTSecret}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected production to refuse the insecure JWT secret")
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsInsecureSecretInDevelopment(t *testing.T) {
	t.Setenv("GSC_ENV", "development")
	cfg := &Config{JWTSecret: DevJWTSecret}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("development should warn but start: %v", err)
	}
}

func TestIsProduction(t *testing.T) {
	t.Setenv("GIN_MODE", "")
	t.Setenv("GSC_ENV", "production")
	if !IsProduction() {
		t.Fatal("GSC_ENV=production")
	}
	t.Setenv("GSC_ENV", "dev")
	if IsProduction() {
		t.Fatal("GSC_ENV=dev should not be production")
	}
	t.Setenv("GSC_ENV", "")
	t.Setenv("GIN_MODE", "release")
	if !IsProduction() {
		t.Fatal("GIN_MODE=release should be production when GSC_ENV is empty")
	}
}

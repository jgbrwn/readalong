package config

import (
	"os"
	"testing"
)

func TestLoopbackAddrIsFixedAndPortConfigurable(t *testing.T) {
	if got := loopbackAddr("8123"); got != "127.0.0.1:8123" {
		t.Fatalf("address = %q", got)
	}
	for _, invalid := range []string{"", "not-a-port", "0", "65536"} {
		if got := loopbackAddr(invalid); got != "127.0.0.1:8000" {
			t.Errorf("loopbackAddr(%q) = %q, want default loopback address", invalid, got)
		}
	}
}

func TestStripDeploymentSecrets(t *testing.T) {
	for _, key := range []string{
		"GROQ_API_KEY", "CLOUDFLARE_API_TOKEN", "R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID",
		"R2_SECRET_ACCESS_KEY", "R2_ENDPOINT",
	} {
		t.Setenv(key, "test-secret")
	}
	stripDeploymentSecrets()
	for _, key := range []string{
		"GROQ_API_KEY", "CLOUDFLARE_API_TOKEN", "R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID",
		"R2_SECRET_ACCESS_KEY", "R2_ENDPOINT",
	} {
		if _, ok := os.LookupEnv(key); ok {
			t.Errorf("%s remains in application environment", key)
		}
	}
}

func TestLoadCapturesGroqKeyBeforeStrippingEnvironment(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("GROQ_API_KEY", "test-only-key")
	cfg := Load()
	if cfg.GroqAPIKey != "test-only-key" {
		t.Fatal("Groq key was not captured in application config")
	}
	if _, ok := os.LookupEnv("GROQ_API_KEY"); ok {
		t.Fatal("Groq key remained in the process environment")
	}
}

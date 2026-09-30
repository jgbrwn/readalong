package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeResolvesRelativeDataDir(t *testing.T) {
	cfg := (Config{DataDir: "./data"}).Normalize()
	want, err := filepath.Abs("./data")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != want {
		t.Fatalf("normalized data dir = %q, want %q", cfg.DataDir, want)
	}
	if got := (Config{}).Normalize(); got.DataDir != "" {
		t.Fatalf("empty data dir became %q", got.DataDir)
	}
}

func TestLoadResolvesRelativeAppDataDir(t *testing.T) {
	t.Setenv("APP_DATA_DIR", "./data")
	cfg := Load()
	want, err := filepath.Abs("./data")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != want {
		t.Fatalf("loaded data dir = %q, want absolute %q", cfg.DataDir, want)
	}
}

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

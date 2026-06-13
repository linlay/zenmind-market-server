package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvReadsValues(t *testing.T) {
	unsetForTest(t, "MARKET_ADDR")
	unsetForTest(t, "MARKET_PUBLIC_BASE_URL")
	unsetForTest(t, "MARKET_MAX_UPLOAD_BYTES")

	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte(`
# comments and empty lines are ignored
export MARKET_ADDR=":9999"
MARKET_PUBLIC_BASE_URL='http://localhost:9999'
MARKET_MAX_UPLOAD_BYTES=1048576
invalid line
1INVALID=value
`), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	if err := loadDotEnv(envPath); err != nil {
		t.Fatalf("loadDotEnv() error = %v", err)
	}
	if got := os.Getenv("MARKET_ADDR"); got != ":9999" {
		t.Fatalf("MARKET_ADDR = %q, want :9999", got)
	}
	if got := os.Getenv("MARKET_PUBLIC_BASE_URL"); got != "http://localhost:9999" {
		t.Fatalf("MARKET_PUBLIC_BASE_URL = %q, want http://localhost:9999", got)
	}
	if got := os.Getenv("MARKET_MAX_UPLOAD_BYTES"); got != "1048576" {
		t.Fatalf("MARKET_MAX_UPLOAD_BYTES = %q, want 1048576", got)
	}
}

func TestLoadDotEnvKeepsExistingEnvironment(t *testing.T) {
	t.Setenv("MARKET_ADDR", ":1234")

	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("MARKET_ADDR=:9999\n"), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	if err := loadDotEnv(envPath); err != nil {
		t.Fatalf("loadDotEnv() error = %v", err)
	}
	if got := os.Getenv("MARKET_ADDR"); got != ":1234" {
		t.Fatalf("MARKET_ADDR = %q, want :1234", got)
	}
}

func unsetForTest(t *testing.T, key string) {
	t.Helper()
	value, exists := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if exists {
			_ = os.Setenv(key, value)
			return
		}
		_ = os.Unsetenv(key)
	})
}

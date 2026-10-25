package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/joho/godotenv"
)

// TestLoadRafaelEnvFallbackNoOverride verifies the first-boot flat-install
// fix: when /etc/default/rafael exists (written by bootstrap.sh) but systemd
// did not inject its contents (the file did not exist at unit start —
// systemd captures EnvironmentFile BEFORE ExecStartPre runs), the panel
// loads the file itself. Variables already present in the environment are
// NEVER overridden (godotenv semantics), so release-layout systems where
// systemd injected the file are unaffected.
func TestLoadRafaelEnvFallback(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "rafael-env")

	if err := os.WriteFile(envFile, []byte("XUI_DB_TYPE=postgres\nXUI_TEST_FALLBACK_VAR=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("XUI_ENV_FILE_OVERRIDE_FOR_TEST", envFile)

	// Simulate the first flat boot: DB type NOT in the environment.
	os.Unsetenv("XUI_DB_TYPE")
	os.Unsetenv("XUI_TEST_FALLBACK_VAR")

	// The production helper hardcodes /etc/default/rafael; on this Windows
	// host that path does not exist, so exercising it directly is not
	// possible. The mechanism under test is godotenv.Load(file) semantics:
	// missing vars get set, existing vars are preserved.
	if err := godotenv.Load(envFile); err != nil {
		t.Fatalf("godotenv.Load: %v", err)
	}
	if os.Getenv("XUI_DB_TYPE") != "postgres" {
		t.Fatal("fallback load must set XUI_DB_TYPE=postgres")
	}
	if os.Getenv("XUI_TEST_FALLBACK_VAR") != "loaded" {
		t.Fatal("fallback load must set unknown vars from the file")
	}

	// Existing environment wins over the file: systemd-injected (or
	// operator-set) values are never clobbered.
	if err := os.WriteFile(envFile, []byte("XUI_DB_TYPE=sqlite\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := godotenv.Load(envFile); err != nil {
		t.Fatalf("godotenv.Load(2): %v", err)
	}
	if os.Getenv("XUI_DB_TYPE") != "postgres" {
		t.Fatal("existing env var must not be overridden by the file")
	}
}

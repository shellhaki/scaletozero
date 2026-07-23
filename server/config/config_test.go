package config

import (
	"testing"
	"time"
)

// setEnv sets an env var for the duration of the test.
func setEnv(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv(key, value)
}

func TestLoadRequiresDockerAPIURL(t *testing.T) {
	setEnv(t, "DOCKER_API_URL", "")
	if _, err := Load(); err == nil {
		t.Error("expected an error when DOCKER_API_URL is unset")
	}
}

func TestLoadAllowsInternalAccessWithoutCredentials(t *testing.T) {
	// Reaching the socket proxy over the Docker network needs no basic auth,
	// which lets the Docker API stay off the public internet entirely.
	setEnv(t, "DOCKER_API_URL", "http://docker-proxy:2375")
	setEnv(t, "DOCKER_API_USERNAME", "")
	setEnv(t, "DOCKER_API_PASSWORD", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DOCKER_API_USERNAME != "" {
		t.Errorf("username = %q, want empty", cfg.DOCKER_API_USERNAME)
	}
}

func TestLoadRejectsHalfConfiguredCredentials(t *testing.T) {
	setEnv(t, "DOCKER_API_URL", "https://docker.example.com")
	setEnv(t, "DOCKER_API_USERNAME", "haki")
	setEnv(t, "DOCKER_API_PASSWORD", "")

	if _, err := Load(); err == nil {
		t.Error("a username without a password must be rejected, not sent unauthenticated")
	}
}

func TestLoadAcceptsFullCredentials(t *testing.T) {
	setEnv(t, "DOCKER_API_URL", "https://docker.example.com")
	setEnv(t, "DOCKER_API_USERNAME", "haki")
	setEnv(t, "DOCKER_API_PASSWORD", "hunter2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DOCKER_API_USERNAME != "haki" || cfg.DOCKER_API_PASSWORD != "hunter2" {
		t.Errorf("credentials = %q/%q", cfg.DOCKER_API_USERNAME, cfg.DOCKER_API_PASSWORD)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	setEnv(t, "DOCKER_API_URL", "http://docker-proxy:2375")
	for _, key := range []string{
		"PORT", "DOCKER_NETWORK", "SQLITE_PATH",
		"IDLE_TIMEOUT", "REAP_INTERVAL", "WAKE_TIMEOUT", "PG_LISTEN_ADDR",
	} {
		setEnv(t, key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.PORT != ":8080" {
		t.Errorf("PORT = %q", cfg.PORT)
	}
	if cfg.PG_LISTEN_ADDR != ":5432" {
		t.Errorf("PG_LISTEN_ADDR = %q, want the standard Postgres port", cfg.PG_LISTEN_ADDR)
	}
	if cfg.DOCKER_NETWORK != "sparkdb-network" {
		t.Errorf("DOCKER_NETWORK = %q", cfg.DOCKER_NETWORK)
	}
	if cfg.IDLE_TIMEOUT != 2*time.Minute {
		t.Errorf("IDLE_TIMEOUT = %s, want 2m", cfg.IDLE_TIMEOUT)
	}
}

func TestLoadParsesDurations(t *testing.T) {
	setEnv(t, "DOCKER_API_URL", "http://docker-proxy:2375")
	setEnv(t, "IDLE_TIMEOUT", "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IDLE_TIMEOUT != 45*time.Second {
		t.Errorf("IDLE_TIMEOUT = %s, want 45s", cfg.IDLE_TIMEOUT)
	}
}

func TestLoadFallsBackOnUnparseableDuration(t *testing.T) {
	setEnv(t, "DOCKER_API_URL", "http://docker-proxy:2375")
	setEnv(t, "IDLE_TIMEOUT", "not-a-duration")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IDLE_TIMEOUT != 2*time.Minute {
		t.Errorf("IDLE_TIMEOUT = %s, want the 2m default", cfg.IDLE_TIMEOUT)
	}
}

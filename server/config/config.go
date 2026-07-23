package config

import (
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// HTTP admin API listen address.
	PORT string

	// Docker Engine API (through the socket proxy).
	//
	// Credentials are optional: when the socket proxy is reached over the
	// internal Docker network it needs no basic auth, and the Docker API is
	// then never exposed publicly. Set both when going through an
	// authenticated public endpoint instead.
	DOCKER_API_URL      string
	DOCKER_API_USERNAME string
	DOCKER_API_PASSWORD string

	// Docker network every provisioned container joins. The proxy dials
	// containers by name on this network, so no host ports are published.
	DOCKER_NETWORK string

	// SQLite file holding database metadata and state.
	SQLITE_PATH string

	// Idle duration after which a running container is scaled to zero.
	IDLE_TIMEOUT time.Duration

	// How often the reaper scans for idle databases.
	REAP_INTERVAL time.Duration

	// How long to wait for a woken container to accept connections.
	WAKE_TIMEOUT time.Duration

	// TCP listen address for the Postgres proxy.
	PG_LISTEN_ADDR string
}

func Load() (*Config, error) {
	_ = godotenv.Load()
	cfg := Config{
		PORT:                getEnv("PORT", ":8080"),
		DOCKER_API_URL:      getEnv("DOCKER_API_URL", ""),
		DOCKER_API_USERNAME: getEnv("DOCKER_API_USERNAME", ""),
		DOCKER_API_PASSWORD: getEnv("DOCKER_API_PASSWORD", ""),
		DOCKER_NETWORK:      getEnv("DOCKER_NETWORK", "sparkdb-network"),
		SQLITE_PATH:         getEnv("SQLITE_PATH", "sparkdb.db"),
		IDLE_TIMEOUT:        getDuration("IDLE_TIMEOUT", 2*time.Minute),
		REAP_INTERVAL:       getDuration("REAP_INTERVAL", 15*time.Second),
		WAKE_TIMEOUT:        getDuration("WAKE_TIMEOUT", 60*time.Second),
		PG_LISTEN_ADDR:      getEnv("PG_LISTEN_ADDR", ":5432"),
	}
	if cfg.DOCKER_API_URL == "" {
		return nil, fmt.Errorf("DOCKER_API_URL is required in environment")
	}
	// Credentials are all-or-nothing: a half-configured pair is a
	// misconfiguration that would silently send unauthenticated requests.
	if (cfg.DOCKER_API_USERNAME == "") != (cfg.DOCKER_API_PASSWORD == "") {
		return nil, fmt.Errorf("DOCKER_API_USERNAME and DOCKER_API_PASSWORD must be set together")
	}
	return &cfg, nil
}

func getEnv(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

func getDuration(key string, defaultValue time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return defaultValue
	}
	return d
}

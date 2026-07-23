package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	PORT                string
	DOCKER_API_URL      string
	DOCKER_API_USERNAME string
	DOCKER_API_PASSWORD string
}

func Load() (*Config, error) {
	_ = godotenv.Load()
	cfg := Config{
		DOCKER_API_URL:      getEnv("DOCKER_API_URL", ""),
		DOCKER_API_USERNAME: getEnv("DOCKER_API_USERNAME", ""),
		DOCKER_API_PASSWORD: getEnv("DOCKER_API_PASSWORD", ""),
	}
	if cfg.DOCKER_API_URL == "" {
		return nil, fmt.Errorf("DOCKER_API_URL is required in environment")
	}
	if cfg.DOCKER_API_USERNAME == "" {
		return nil, fmt.Errorf("DOCKER_API_USERNAME is required in environment")
	}
	if cfg.DOCKER_API_PASSWORD == "" {
		return nil, fmt.Errorf("DOCKER_API_PASSWORD is required in environment")
	}
	return &cfg, nil
}

func getEnv(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

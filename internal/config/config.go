package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	ServerPort     string
	TwitchChannel  string
	YouTubeAPIKey  string
	YouTubeChannel string
	KickChannel    string
}

// Load reads configuration from the environment, loading a .env file if one exists.
// A .env file is optional — variables can also be injected directly (e.g. in Docker/systemd).
func Load() (*Config, error) {
	// Silently ignore a missing .env so production environments that inject
	// variables directly (Docker, systemd, etc.) work without modification.
	_ = godotenv.Load()

	cfg := &Config{
		ServerPort:     getEnv("SERVER_PORT", "8080"),
		TwitchChannel:  os.Getenv("TWITCH_CHANNEL"),
		YouTubeAPIKey:  os.Getenv("YOUTUBE_API_KEY"),
		YouTubeChannel: os.Getenv("YOUTUBE_CHANNEL"),
		KickChannel:    os.Getenv("KICK_CHANNEL"),
	}

	if !cfg.hasAnyProvider() {
		return nil, fmt.Errorf(
			"no providers configured: set at least one of TWITCH_CHANNEL, YOUTUBE_CHANNEL (+ YOUTUBE_API_KEY), or KICK_CHANNEL",
		)
	}

	return cfg, nil
}

// hasAnyProvider returns true if at least one provider has enough config to start.
func (c *Config) hasAnyProvider() bool {
	return c.TwitchChannel != "" ||
		(c.YouTubeAPIKey != "" && c.YouTubeChannel != "") ||
		c.KickChannel != ""
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Mailbox struct {
		Enabled               bool
		SpoolDir              string
		PendingTimeoutSeconds int
		KeepaliveSeconds      int
		PollIntervalMillis    int
	}
	Gateway struct {
		MaxBodySize int64
	}
	APIKey string
	Server struct {
		Host string
		Port int
	}
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func LoadFromEnv() *Config {
	cfg := &Config{}
	cfg.Mailbox.Enabled = true
	cfg.Mailbox.SpoolDir = strings.TrimSpace(os.Getenv("MAILBOX_SPOOL_DIR"))
	cfg.Mailbox.PendingTimeoutSeconds = envInt("MAILBOX_PENDING_TIMEOUT_SECONDS", 600)
	cfg.Mailbox.KeepaliveSeconds = envInt("MAILBOX_KEEPALIVE_SECONDS", 15)
	cfg.Mailbox.PollIntervalMillis = envInt("MAILBOX_POLL_INTERVAL_MILLIS", 500)
	cfg.Gateway.MaxBodySize = int64(envInt("GATEWAY_MAX_BODY_SIZE", 268435456))
	cfg.APIKey = strings.TrimSpace(os.Getenv("MAILBOX_API_KEY"))
	if cfg.APIKey == "" {
		cfg.APIKey = "sk-mailbox-local"
	}
	cfg.Server.Host = strings.TrimSpace(os.Getenv("SERVER_HOST"))
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	cfg.Server.Port = envInt("SERVER_PORT", 8080)
	return cfg
}

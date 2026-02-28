// Package config loads and validates all environment variables at startup.
// Every other package receives typed values — nothing reads os.Getenv directly.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-parsed application configuration.
type Config struct {
	// ── Server ────────────────────────────────────────────────────────────────
	Port    string // default "8080"
	Env     string // "development" | "staging" | "production"
	BaseURL string // e.g. "https://app.asymmetricrisk.com"

	// ── Database ──────────────────────────────────────────────────────────────
	DatabaseURL string // postgres://user:pass@host:5432/dbname?sslmode=require

	// ── Stripe ────────────────────────────────────────────────────────────────
	StripeSecretKey     string
	StripeWebhookSecret string

	// ── Anthropic ─────────────────────────────────────────────────────────────
	AnthropicAPIKey string
	AnthropicModel  string // default "claude-opus-4-6"

	// ── DeepSeek ──────────────────────────────────────────────────────────────
	// Optional. When set, DeepSeek is used as the fallback if the Anthropic
	// call fails. If DEEPSEEK_API_KEY is empty, no fallback is configured.
	DeepSeekAPIKey string
	DeepSeekModel  string // default "deepseek-chat"

	// ── Resend ────────────────────────────────────────────────────────────────
	ResendAPIKey  string
	EmailFromAddr string // e.g. "reports@asymmetricrisk.com"
	EmailFromName string // e.g. "Asymmetric Risk"

	// ── Worker ────────────────────────────────────────────────────────────────
	WorkerCount  int           // default 3
	PollInterval time.Duration // default 30s
	JobTimeout   time.Duration // default 5m
	MaxRetries   int           // default 3
}

// Load reads all environment variables and returns a validated Config.
// It automatically loads a .env file from the working directory when present,
// so plain `go run ./cmd/api` works in development without any wrapper.
// Real environment variables always take precedence over .env values.
func Load() (*Config, error) {
	loadDotEnv(".env")

	c := &Config{
		Port:                getEnv("PORT", "8080"),
		Env:                 getEnv("ENV", "development"),
		BaseURL:             getEnv("BASE_URL", "http://localhost:8080"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		StripeSecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		AnthropicAPIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:      getEnv("ANTHROPIC_MODEL", "claude-opus-4-6"),
		DeepSeekAPIKey:      os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekModel:       getEnv("DEEPSEEK_MODEL", "deepseek-chat"),
		ResendAPIKey:        os.Getenv("RESEND_API_KEY"),
		EmailFromAddr:       getEnv("EMAIL_FROM_ADDR", "reports@asymmetricrisk.com"),
		EmailFromName:       getEnv("EMAIL_FROM_NAME", "Asymmetric Risk"),
		WorkerCount:         getEnvAsInt("WORKER_COUNT", 3),
		PollInterval:        getEnvAsDuration("POLL_INTERVAL", 30*time.Second),
		JobTimeout:          getEnvAsDuration("JOB_TIMEOUT", 5*time.Minute),
		MaxRetries:          getEnvAsInt("MAX_RETRIES", 3),
	}

	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error

	required := map[string]string{
		"DATABASE_URL":      c.DatabaseURL,
		"STRIPE_SECRET_KEY": c.StripeSecretKey,
		"RESEND_API_KEY":    c.ResendAPIKey,
	}

	for name, val := range required {
		if val == "" {
			errs = append(errs, fmt.Errorf("missing required env var: %s", name))
		}
	}

	// At least one AI provider must be configured.
	if c.AnthropicAPIKey == "" && c.DeepSeekAPIKey == "" {
		errs = append(errs, fmt.Errorf("at least one of ANTHROPIC_API_KEY or DEEPSEEK_API_KEY must be set"))
	}

	return errors.Join(errs...)
}

// ─── DOT-ENV LOADER ──────────────────────────────────────────────────────────

// loadDotEnv reads key=value pairs from path and sets them in the environment,
// but only for keys that are not already set. This means real env vars (e.g.
// from Docker / Railway / your shell) always win over the file.
// Missing file, blank lines, and #-comments are all silently ignored.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // file absent — that's fine
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// Strip optional surrounding quotes: KEY="value" or KEY='value'
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		// Only set if the key isn't already present in the environment.
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

// ─── HELPERS ─────────────────────────────────────────────────────────────────

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	if value, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return value
	}
	return defaultValue
}

func getEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	// Try a plain integer first (treated as seconds, minutes, or hours
	// depending on the variable name).
	if value, err := strconv.Atoi(valueStr); err == nil {
		switch {
		case strings.Contains(key, "HOURS"):
			return time.Duration(value) * time.Hour
		case strings.Contains(key, "MINUTES"):
			return time.Duration(value) * time.Minute
		default:
			return time.Duration(value) * time.Second
		}
	}
	// Fall back to Go duration syntax: "30s", "5m", "1h", etc.
	if duration, err := time.ParseDuration(valueStr); err == nil {
		return duration
	}
	return defaultValue
}

func getEnvAsBool(key string, defaultValue bool) bool {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.ParseBool(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

// Package config loads and validates all environment variables at startup.
// Every other package receives typed values — nothing reads os.Getenv directly.
package config

import (
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
	ResendAPIKey    string
	EmailFromAddr   string // e.g. "reports@asymmetricrisk.com"
	EmailFromName   string // e.g. "Asymmetric Risk"

	// ── Worker ────────────────────────────────────────────────────────────────
	WorkerCount  int           // default 3
	PollInterval time.Duration // default 30s
	JobTimeout   time.Duration // default 5m
	MaxRetries   int           // default 3
}

// Load reads all environment variables and returns a validated Config.
// Returns an error listing every missing required variable so you fix them all
// at once rather than one at a time.
func Load() (*Config, error) {
	c := &Config{
		Port:            getEnv("PORT", "8080"),
		Env:             getEnv("ENV", "development"),
		BaseURL:         getEnv("BASE_URL", "http://localhost:8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		StripeSecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		AnthropicAPIKey: os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:  getEnv("ANTHROPIC_MODEL", "claude-opus-4-6"),
		DeepSeekAPIKey:  os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekModel:   getEnv("DEEPSEEK_MODEL", "deepseek-chat"),
		ResendAPIKey:    os.Getenv("RESEND_API_KEY"),
		EmailFromAddr:   getEnv("EMAIL_FROM_ADDR", "reports@asymmetricrisk.com"),
		EmailFromName:   getEnv("EMAIL_FROM_NAME", "Asymmetric Risk"),
		WorkerCount:     getEnvAsInt("WORKER_COUNT", 3),
		PollInterval:    getEnvAsDuration("POLL_INTERVAL", 30*time.Second),
		JobTimeout:      getEnvAsDuration("JOB_TIMEOUT", 5*time.Minute),
		MaxRetries:      getEnvAsInt("MAX_RETRIES", 3),
	}

	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error

	required := map[string]string{
		"DATABASE_URL":          c.DatabaseURL,
		"STRIPE_SECRET_KEY":     c.StripeSecretKey,
		"RESEND_API_KEY":        c.ResendAPIKey,
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

// ─── HELPERS ─────────────────────────────────────────────────────────────────

// func getEnv(key, fallback string) string {
// 	if v := os.Getenv(key); v != "" {
// 		return v
// 	}
// 	return fallback
// }

func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// getEnv gets an environment variable or returns default
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvAsInt gets an environment variable as int or returns default
func getEnvAsInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if value, err := strconv.Atoi(valueStr); err == nil {
		return value
	}
	return defaultValue
}

// getEnvAsDuration gets an environment variable as duration or returns default
func getEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}

	// Try parsing as integer (seconds/hours depending on key)
	if value, err := strconv.Atoi(valueStr); err == nil {
		if strings.Contains(key, "HOURS") {
			return time.Duration(value) * time.Hour
		} else if strings.Contains(key, "MINUTES") {
			return time.Duration(value) * time.Minute
		}
		return time.Duration(value) * time.Second
	}

	// Try parsing as duration string
	if duration, err := time.ParseDuration(valueStr); err == nil {
		return duration
	}

	return defaultValue
}


// getEnvAsBool gets an environment variable as bool or returns default
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
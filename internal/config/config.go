package config

import (
	"errors"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPAddr            = "127.0.0.1:8080"
	defaultRateLimitRPS        = 10.0
	defaultRateLimitBurst      = 20
	defaultRateLimitEnabled    = true
	defaultWorkerCount         = 2
	defaultWorkerPollInterval  = 500 * time.Millisecond
	defaultWorkerLeaseDuration = 30 * time.Second
	defaultLLMModel            = "gpt-4o-mini"
	defaultLLMTimeout          = 30 * time.Second
)

// TriageLLMConfig holds optional LLM provider settings for asynchronous incident triage.
// APIKey is sensitive and must never be logged.
type TriageLLMConfig struct {
	APIKey   string
	Endpoint string
	Model    string
	Timeout  time.Duration
}

// WorkerConfig configures the durable asynchronous incident worker pool.
type WorkerConfig struct {
	Count         int
	PollInterval  time.Duration
	LeaseDuration time.Duration
}

// RateLimitConfig contains incoming API rate limiting parameters.
type RateLimitConfig struct {
	Enabled        bool
	RequestsPerSec float64
	BurstCapacity  int
	TrustedProxies []string
}

// Config contains only runtime settings. DatabaseURL and TriageLLM.APIKey are sensitive and must not be logged.
type Config struct {
	DatabaseURL      string
	HTTPAddr         string
	RateLimit        RateLimitConfig
	PreAuthRateLimit RateLimitConfig
	Worker           WorkerConfig
	TriageLLM        TriageLLMConfig
}

// Load reads configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom makes environment parsing deterministic and straightforward to test.
func LoadFrom(getenv func(string) string) (Config, error) {
	databaseURL := strings.TrimSpace(getenv("SENTINEL_DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, errors.New("SENTINEL_DATABASE_URL is required")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" || strings.Trim(parsed.Path, "/") == "" {
		return Config{}, errors.New("SENTINEL_DATABASE_URL must be a PostgreSQL URL with a host and database name")
	}

	httpAddr := strings.TrimSpace(getenv("SENTINEL_HTTP_ADDR"))
	if httpAddr == "" {
		httpAddr = defaultHTTPAddr
	}
	_, portText, err := net.SplitHostPort(httpAddr)
	if err != nil {
		return Config{}, errors.New("SENTINEL_HTTP_ADDR must be a valid host:port address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Config{}, errors.New("SENTINEL_HTTP_ADDR must contain a port between 1 and 65535")
	}

	rateLimitEnabled := defaultRateLimitEnabled
	if val := strings.TrimSpace(getenv("SENTINEL_RATE_LIMIT_ENABLED")); val != "" {
		parsedBool, err := strconv.ParseBool(val)
		if err != nil {
			return Config{}, errors.New("SENTINEL_RATE_LIMIT_ENABLED must be a valid boolean")
		}
		rateLimitEnabled = parsedBool
	}

	rateLimitRPS := defaultRateLimitRPS
	if val := strings.TrimSpace(getenv("SENTINEL_RATE_LIMIT_RPS")); val != "" {
		parsedRPS, err := strconv.ParseFloat(val, 64)
		if err != nil || math.IsNaN(parsedRPS) || math.IsInf(parsedRPS, 0) || parsedRPS <= 0 {
			return Config{}, errors.New("SENTINEL_RATE_LIMIT_RPS must be a finite positive number")
		}
		rateLimitRPS = parsedRPS
	}

	rateLimitBurst := defaultRateLimitBurst
	if val := strings.TrimSpace(getenv("SENTINEL_RATE_LIMIT_BURST")); val != "" {
		parsedBurst, err := strconv.Atoi(val)
		if err != nil || parsedBurst <= 0 {
			return Config{}, errors.New("SENTINEL_RATE_LIMIT_BURST must be a positive integer")
		}
		rateLimitBurst = parsedBurst
	}

	var trustedProxies []string
	if val := strings.TrimSpace(getenv("SENTINEL_RATE_LIMIT_TRUSTED_PROXIES")); val != "" {
		for _, p := range strings.Split(val, ",") {
			trimmed := strings.TrimSpace(p)
			if trimmed != "" {
				trustedProxies = append(trustedProxies, trimmed)
			}
		}
	}

	preAuthRPS := rateLimitRPS
	if val := strings.TrimSpace(getenv("SENTINEL_PREAUTH_RATE_LIMIT_RPS")); val != "" {
		parsedRPS, err := strconv.ParseFloat(val, 64)
		if err != nil || math.IsNaN(parsedRPS) || math.IsInf(parsedRPS, 0) || parsedRPS <= 0 {
			return Config{}, errors.New("SENTINEL_PREAUTH_RATE_LIMIT_RPS must be a finite positive number")
		}
		preAuthRPS = parsedRPS
	}

	preAuthBurst := rateLimitBurst
	if val := strings.TrimSpace(getenv("SENTINEL_PREAUTH_RATE_LIMIT_BURST")); val != "" {
		parsedBurst, err := strconv.Atoi(val)
		if err != nil || parsedBurst <= 0 {
			return Config{}, errors.New("SENTINEL_PREAUTH_RATE_LIMIT_BURST must be a positive integer")
		}
		preAuthBurst = parsedBurst
	}

	workerCount := defaultWorkerCount
	if val := strings.TrimSpace(getenv("SENTINEL_WORKER_COUNT")); val != "" {
		parsedCount, err := strconv.Atoi(val)
		if err != nil || parsedCount < 0 {
			return Config{}, errors.New("SENTINEL_WORKER_COUNT must be a non-negative integer")
		}
		workerCount = parsedCount
	}

	workerPoll := defaultWorkerPollInterval
	if val := strings.TrimSpace(getenv("SENTINEL_WORKER_POLL_INTERVAL")); val != "" {
		parsedPoll, err := time.ParseDuration(val)
		if err != nil || parsedPoll <= 0 {
			return Config{}, errors.New("SENTINEL_WORKER_POLL_INTERVAL must be a positive duration")
		}
		workerPoll = parsedPoll
	}

	workerLease := defaultWorkerLeaseDuration
	if val := strings.TrimSpace(getenv("SENTINEL_WORKER_LEASE_DURATION")); val != "" {
		parsedLease, err := time.ParseDuration(val)
		if err != nil || parsedLease <= 0 {
			return Config{}, errors.New("SENTINEL_WORKER_LEASE_DURATION must be a positive duration")
		}
		workerLease = parsedLease
	}

	llmAPIKey := strings.TrimSpace(getenv("SENTINEL_LLM_API_KEY"))
	llmEndpoint := strings.TrimSpace(getenv("SENTINEL_LLM_ENDPOINT"))
	llmModel := strings.TrimSpace(getenv("SENTINEL_LLM_MODEL"))
	if llmModel == "" {
		llmModel = defaultLLMModel
	}
	llmTimeout := defaultLLMTimeout
	if val := strings.TrimSpace(getenv("SENTINEL_LLM_TIMEOUT")); val != "" {
		parsedTimeout, err := time.ParseDuration(val)
		if err != nil || parsedTimeout < time.Second || parsedTimeout > 2*time.Minute {
			return Config{}, errors.New("SENTINEL_LLM_TIMEOUT must be a duration between 1s and 2m")
		}
		llmTimeout = parsedTimeout
	}

	return Config{
		DatabaseURL: databaseURL,
		HTTPAddr:    httpAddr,
		RateLimit: RateLimitConfig{
			Enabled:        rateLimitEnabled,
			RequestsPerSec: rateLimitRPS,
			BurstCapacity:  rateLimitBurst,
			TrustedProxies: trustedProxies,
		},
		PreAuthRateLimit: RateLimitConfig{
			Enabled:        rateLimitEnabled,
			RequestsPerSec: preAuthRPS,
			BurstCapacity:  preAuthBurst,
			TrustedProxies: trustedProxies,
		},
		Worker: WorkerConfig{
			Count:         workerCount,
			PollInterval:  workerPoll,
			LeaseDuration: workerLease,
		},
		TriageLLM: TriageLLMConfig{
			APIKey:   llmAPIKey,
			Endpoint: llmEndpoint,
			Model:    llmModel,
			Timeout:  llmTimeout,
		},
	}, nil
}

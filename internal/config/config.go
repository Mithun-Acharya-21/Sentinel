package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const defaultHTTPAddr = "127.0.0.1:8080"

// Config contains only runtime settings. DatabaseURL is sensitive and must not be logged.
type Config struct {
	DatabaseURL string
	HTTPAddr    string
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

	return Config{DatabaseURL: databaseURL, HTTPAddr: httpAddr}, nil
}

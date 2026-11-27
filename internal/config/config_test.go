package config

import (
	"strings"
	"testing"
)

func TestLoadFromRequiresValidDatabaseURL(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
	}{
		{name: "missing"},
		{name: "wrong scheme", dsn: "http://localhost/sentinel"},
		{name: "missing host", dsn: "postgres:///sentinel"},
		{name: "missing database", dsn: "postgres://localhost/"},
		{name: "malformed", dsn: "://postgres"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFrom(func(key string) string {
				if key == "SENTINEL_DATABASE_URL" {
					return tt.dsn
				}
				return ""
			})
			if err == nil {
				t.Fatal("LoadFrom succeeded for invalid database URL")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error exposed connection details: %v", err)
			}
		})
	}
}

func TestLoadFromUsesDefaultsAndConfiguredAddress(t *testing.T) {
	getenv := func(key string) string {
		switch key {
		case "SENTINEL_DATABASE_URL":
			return "postgres://sentinel:secret@127.0.0.1:5435/sentinel?sslmode=disable"
		default:
			return ""
		}
	}
	cfg, err := LoadFrom(getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, defaultHTTPAddr)
	}
	if cfg.DatabaseURL != getenv("SENTINEL_DATABASE_URL") {
		t.Fatal("database URL was not preserved for the driver")
	}

	cfg, err = LoadFrom(func(key string) string {
		if key == "SENTINEL_DATABASE_URL" {
			return "postgres://localhost/sentinel"
		}
		if key == "SENTINEL_HTTP_ADDR" {
			return "127.0.0.1:9090"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Fatalf("HTTPAddr = %q", cfg.HTTPAddr)
	}
}

func TestLoadFromRejectsInvalidHTTPAddress(t *testing.T) {
	for _, addr := range []string{"localhost", ":abc", "localhost:70000"} {
		t.Run(addr, func(t *testing.T) {
			_, err := LoadFrom(func(key string) string {
				if key == "SENTINEL_DATABASE_URL" {
					return "postgres://localhost/sentinel"
				}
				if key == "SENTINEL_HTTP_ADDR" {
					return addr
				}
				return ""
			})
			if err == nil {
				t.Fatal("LoadFrom accepted invalid HTTP address")
			}
		})
	}
}

func TestLoadFromRateLimitSettings(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := LoadFrom(func(key string) string {
			if key == "SENTINEL_DATABASE_URL" {
				return "postgres://localhost/sentinel"
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.RateLimit.Enabled {
			t.Fatal("RateLimit.Enabled should default to true")
		}
		if cfg.RateLimit.RequestsPerSec != 10.0 {
			t.Fatalf("RequestsPerSec = %f, want 10.0", cfg.RateLimit.RequestsPerSec)
		}
		if cfg.RateLimit.BurstCapacity != 20 {
			t.Fatalf("BurstCapacity = %d, want 20", cfg.RateLimit.BurstCapacity)
		}
		if len(cfg.RateLimit.TrustedProxies) != 0 {
			t.Fatalf("TrustedProxies = %v, want empty", cfg.RateLimit.TrustedProxies)
		}
	})

	t.Run("configured values", func(t *testing.T) {
		cfg, err := LoadFrom(func(key string) string {
			switch key {
			case "SENTINEL_DATABASE_URL":
				return "postgres://localhost/sentinel"
			case "SENTINEL_RATE_LIMIT_ENABLED":
				return "false"
			case "SENTINEL_RATE_LIMIT_RPS":
				return "25.5"
			case "SENTINEL_RATE_LIMIT_BURST":
				return "50"
			case "SENTINEL_RATE_LIMIT_TRUSTED_PROXIES":
				return "10.0.0.1, 192.168.0.0/16"
			default:
				return ""
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RateLimit.Enabled {
			t.Fatal("RateLimit.Enabled should be false")
		}
		if cfg.RateLimit.RequestsPerSec != 25.5 {
			t.Fatalf("RequestsPerSec = %f, want 25.5", cfg.RateLimit.RequestsPerSec)
		}
		if cfg.RateLimit.BurstCapacity != 50 {
			t.Fatalf("BurstCapacity = %d, want 50", cfg.RateLimit.BurstCapacity)
		}
		if len(cfg.RateLimit.TrustedProxies) != 2 || cfg.RateLimit.TrustedProxies[0] != "10.0.0.1" || cfg.RateLimit.TrustedProxies[1] != "192.168.0.0/16" {
			t.Fatalf("TrustedProxies = %v", cfg.RateLimit.TrustedProxies)
		}
	})

	t.Run("invalid RPS rejected", func(t *testing.T) {
		for _, rps := range []string{"0", "-5", "invalid", "NaN", "Inf", "-Inf"} {
			_, err := LoadFrom(func(key string) string {
				if key == "SENTINEL_DATABASE_URL" {
					return "postgres://localhost/sentinel"
				}
				if key == "SENTINEL_RATE_LIMIT_RPS" {
					return rps
				}
				return ""
			})
			if err == nil {
				t.Fatalf("LoadFrom accepted invalid RPS %q", rps)
			}
		}
	})

	t.Run("invalid burst rejected", func(t *testing.T) {
		for _, burst := range []string{"0", "-1", "ten"} {
			_, err := LoadFrom(func(key string) string {
				if key == "SENTINEL_DATABASE_URL" {
					return "postgres://localhost/sentinel"
				}
				if key == "SENTINEL_RATE_LIMIT_BURST" {
					return burst
				}
				return ""
			})
			if err == nil {
				t.Fatalf("LoadFrom accepted invalid burst %q", burst)
			}
		}
	})
}

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

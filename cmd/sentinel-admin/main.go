package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"sentinel/internal/auth"
	"sentinel/internal/config"
	"sentinel/internal/storage/postgres"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const dbTimeout = 10 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "sentinel-admin error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("subcommand required: create-key or revoke-key")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return errors.New("could not configure PostgreSQL driver")
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return errors.New("PostgreSQL connection failed; verify SENTINEL_DATABASE_URL")
	}

	store := postgres.NewCredentialRepository(db)

	switch args[0] {
	case "create-key":
		return runCreateKey(ctx, store, args[1:])
	case "revoke-key":
		return runRevokeKey(ctx, store, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q: expected create-key or revoke-key", args[0])
	}
}

func runCreateKey(ctx context.Context, store auth.CredentialStore, args []string) error {
	fs := flag.NewFlagSet("create-key", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "authoritative tenant ID")
	principalID := fs.String("principal", "", "principal identifier")
	scopesRaw := fs.String("scopes", auth.ScopeIncidentsRead+","+auth.ScopeIncidentsWrite, "comma-separated scopes")
	expiresIn := fs.Duration("expires-in", 0, "optional credential lifetime duration (e.g. 720h)")
	expiresAtRaw := fs.String("expires-at", "", "optional RFC3339 UTC expiration timestamp (e.g. 2026-12-31T23:59:59Z)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *expiresIn != 0 && strings.TrimSpace(*expiresAtRaw) != "" {
		return errors.New("cannot specify both -expires-in and -expires-at")
	}
	if *expiresIn < 0 {
		return errors.New("-expires-in must be positive")
	}

	var expiresAt *time.Time
	if *expiresIn > 0 {
		t := time.Now().UTC().Add(*expiresIn)
		expiresAt = &t
	} else if trimmedExp := strings.TrimSpace(*expiresAtRaw); trimmedExp != "" {
		parsed, err := time.Parse(time.RFC3339, trimmedExp)
		if err != nil {
			return fmt.Errorf("invalid -expires-at timestamp (expected RFC3339): %w", err)
		}
		t := parsed.UTC()
		if !t.After(time.Now().UTC()) {
			return errors.New("-expires-at must be in the future")
		}
		expiresAt = &t
	}

	var scopes []string
	for _, s := range strings.Split(*scopesRaw, ",") {
		trimmed := strings.TrimSpace(s)
		if trimmed != "" {
			scopes = append(scopes, trimmed)
		}
	}

	provisioned, input, err := auth.GenerateAPIKeyWithExpiry(*tenantID, *principalID, scopes, expiresAt)
	if err != nil {
		return err
	}

	stored, err := store.Create(ctx, input)
	if err != nil {
		return err
	}

	expText := "never"
	if stored.ExpiresAt != nil {
		expText = stored.ExpiresAt.UTC().Format(time.RFC3339)
	}

	fmt.Printf("Credential ID: %s\n", stored.ID)
	fmt.Printf("Key ID:        %s\n", stored.KeyID)
	fmt.Printf("Tenant ID:     %s\n", stored.TenantID)
	fmt.Printf("Principal ID:  %s\n", stored.PrincipalID)
	fmt.Printf("Scopes:        %s\n", strings.Join(stored.Scopes, ","))
	fmt.Printf("Expires At:    %s\n", expText)
	fmt.Printf("API Key (shown once, store securely):\n%s\n", provisioned.PlaintextToken)
	return nil
}

func runRevokeKey(ctx context.Context, store auth.CredentialStore, args []string) error {
	fs := flag.NewFlagSet("revoke-key", flag.ContinueOnError)
	keyID := fs.String("key-id", "", "public key_id to revoke (e.g. snt_...)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*keyID) == "" {
		return errors.New("-key-id is required")
	}
	if err := store.Revoke(ctx, strings.TrimSpace(*keyID)); err != nil {
		return err
	}
	fmt.Printf("Revoked credential: %s\n", strings.TrimSpace(*keyID))
	return nil
}

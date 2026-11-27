package auth

import (
	"context"
	"strings"
)

// Supported authorization scopes for Sentinel's incident API.
const (
	ScopeIncidentsRead    = "incidents:read"
	ScopeIncidentsWrite   = "incidents:write"
	ScopeIncidentsApprove = "incidents:approve"
	ScopeIncidentsExecute = "incidents:execute"
)

// AuthMethodBearerAPIKey identifies credentials verified via opaque Bearer API keys.
const AuthMethodBearerAPIKey = "bearer_api_key"

// Principal represents an authenticated caller identity established exclusively by auth middleware.
type Principal struct {
	PrincipalID  string
	TenantID     string
	CredentialID string
	Scopes       []string
	AuthMethod   string
}

// HasScope reports whether the principal has been granted the required scope.
func (p Principal) HasScope(required string) bool {
	for _, s := range p.Scopes {
		if strings.TrimSpace(s) == required {
			return true
		}
	}
	return false
}

type principalKey struct{}
type authErrorKey struct{}

// WithPrincipal attaches a verified Principal to the request context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext retrieves the verified Principal from context if present.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok || strings.TrimSpace(p.TenantID) == "" || strings.TrimSpace(p.PrincipalID) == "" {
		return Principal{}, false
	}
	return p, true
}

// WithAuthError records a credential verification failure on the request context
// so downstream authorization middleware can distinguish invalid/revoked credentials
// from missing credentials after rate limiting.
func WithAuthError(ctx context.Context, err error) context.Context {
	return context.WithValue(ctx, authErrorKey{}, err)
}

// AuthErrorFromContext retrieves a recorded credential verification error from context.
func AuthErrorFromContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	err, _ := ctx.Value(authErrorKey{}).(error)
	return err
}

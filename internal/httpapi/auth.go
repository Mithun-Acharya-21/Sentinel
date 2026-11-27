package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"sentinel/internal/auth"
)

type requestIDContextKey struct{}

// RequestIDFromContext returns the request ID attached by RequestMetadataMiddleware.
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

// RequestMetadataMiddleware ensures every request carries a sanitized X-Request-ID
// for correlation across rate limiting, authentication, and storage logs.
func RequestMetadataMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqID := sanitizeRequestID(r.Header.Get("X-Request-ID"))
			if reqID == "" {
				var buf [16]byte
				if _, err := rand.Read(buf[:]); err == nil {
					reqID = hex.EncodeToString(buf[:])
				}
			}
			if reqID != "" {
				w.Header().Set("X-Request-ID", reqID)
				r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, reqID))
			}
			next.ServeHTTP(w, r)
		})
	}
}

func sanitizeRequestID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > 64 {
		return ""
	}
	for _, c := range trimmed {
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlphaNum && c != '-' && c != '_' {
			return ""
		}
	}
	return trimmed
}

// AuthenticateMiddleware resolves Bearer credentials into a verified Principal and
// populates WithVerifiedPrincipal for downstream rate limiting without blocking
// unauthenticated requests before the rate limiter can throttle them.
func AuthenticateMiddleware(verifier auth.CredentialVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health probes remain unauthenticated and unthrottled.
			if r.URL.Path == "/health/live" || r.URL.Path == "/health/ready" {
				next.ServeHTTP(w, r)
				return
			}

			authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
			if authHeader == "" {
				next.ServeHTTP(w, r)
				return
			}

			if len(authHeader) < 7 || !strings.EqualFold(authHeader[:7], "bearer ") {
				r = r.WithContext(auth.WithAuthError(r.Context(), auth.ErrInvalidCredentials))
				next.ServeHTTP(w, r)
				return
			}

			token := strings.TrimSpace(authHeader[7:])
			if token == "" {
				r = r.WithContext(auth.WithAuthError(r.Context(), auth.ErrInvalidCredentials))
				next.ServeHTTP(w, r)
				return
			}

			if verifier == nil {
				r = r.WithContext(auth.WithAuthError(r.Context(), auth.ErrVerifierUnavailable))
				next.ServeHTTP(w, r)
				return
			}

			principal, err := verifier.Verify(r.Context(), token)
			if err != nil {
				r = r.WithContext(auth.WithAuthError(r.Context(), err))
				next.ServeHTTP(w, r)
				return
			}

			ctx := auth.WithPrincipal(r.Context(), principal)
			ctx = WithVerifiedPrincipal(ctx, principal.PrincipalID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAuthMiddleware rejects unauthenticated or invalid-credential requests after
// rate limiting has had the opportunity to throttle brute-force traffic.
func RequireAuthMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := auth.PrincipalFromContext(r.Context()); ok {
				next.ServeHTTP(w, r)
				return
			}

			authErr := auth.AuthErrorFromContext(r.Context())
			if errors.Is(authErr, auth.ErrVerifierUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "authentication service is temporarily unavailable")
				return
			}

			w.Header().Set("WWW-Authenticate", `Bearer realm="sentinel"`)
			if authErr != nil {
				writeError(w, http.StatusUnauthorized, "invalid_credentials", "credentials are invalid or revoked")
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer credentials are required")
		})
	}
}

// RequireScopeMiddleware enforces that the authenticated principal holds the required scope.
func RequireScopeMiddleware(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, ok := auth.PrincipalFromContext(r.Context())
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="sentinel"`)
				writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer credentials are required")
				return
			}
			if !principal.HasScope(scope) {
				writeError(w, http.StatusForbidden, "forbidden", "authenticated principal lacks required permission")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

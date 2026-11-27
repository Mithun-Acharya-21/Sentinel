package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sentinel/internal/auth"
)

// Default rate limiting configuration parameters.
const (
	DefaultRequestsPerSecond = 10.0
	DefaultBurstCapacity     = 20
	DefaultBucketTTL         = 10 * time.Minute
	DefaultCleanupInterval   = 1 * time.Minute
	DefaultMaxBuckets        = 10000
)

const maxRetryAfterSeconds = float64(math.MaxInt64 / int64(time.Second))

type principalContextKey struct{}

// WithVerifiedPrincipal attaches an authenticated principal identifier to the request context.
// Only verified authentication middleware should call this; unverified request headers are never trusted.
func WithVerifiedPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// RateLimitConfig configures the incoming HTTP rate limiter.
type RateLimitConfig struct {
	Enabled         bool
	RequestsPerSec  float64
	BurstCapacity   int
	BucketTTL       time.Duration
	CleanupInterval time.Duration
	MaxBuckets      int
	Clock           func() time.Time
	TrustedProxies  []string
}

// LimiterMetrics holds real-time telemetry on rate limiting decisions.
type LimiterMetrics struct {
	AllowedRequests  uint64  `json:"allowed_requests"`
	RejectedRequests uint64  `json:"rejected_requests"`
	ActiveBuckets    int     `json:"active_buckets"`
	RejectionRate    float64 `json:"rejection_rate"`
}

// tokenBucket tracks token reserves and refill timing for a single client key.
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	capacity   float64
	refillRate float64
	lastRefill time.Time
	lastSeen   time.Time
}

func newTokenBucket(capacity int, refillRate float64, now time.Time) *tokenBucket {
	// A new bucket starts with capacity - 1 tokens because the first request
	// immediately consumes one token upon admission.
	capF := float64(capacity)
	initialTokens := capF - 1.0
	if initialTokens < 0 {
		initialTokens = 0
	}
	return &tokenBucket{
		tokens:     initialTokens,
		capacity:   capF,
		refillRate: refillRate,
		lastRefill: now,
		lastSeen:   now,
	}
}

// allow evaluates whether a request can be admitted and computes the wait duration if rejected.
func (b *tokenBucket) allow(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Refill tokens based on elapsed wall/virtual clock time.
	// If the clock moved backward (elapsed <= 0), do not subtract tokens or move lastRefill backward.
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed > 0 {
		refilled := b.tokens + elapsed*b.refillRate
		if math.IsInf(refilled, 1) || refilled > b.capacity {
			b.tokens = b.capacity
		} else {
			b.tokens = refilled
		}
		b.lastRefill = now
	}
	if now.After(b.lastSeen) {
		b.lastSeen = now
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		if b.tokens < 0 {
			b.tokens = 0
		}
		return true, 0
	}

	// Calculate wait duration until at least 1 token is available.
	deficit := math.Max(0.0, 1.0-b.tokens)
	waitSec := math.Ceil(deficit / b.refillRate)
	if math.IsNaN(waitSec) || waitSec < 1.0 {
		waitSec = 1.0
	} else if waitSec > maxRetryAfterSeconds {
		waitSec = maxRetryAfterSeconds
	}
	return false, time.Duration(waitSec) * time.Second
}

// Limiter manages per-client token buckets, eviction, metrics, and admission control.
type Limiter struct {
	mu            sync.RWMutex
	buckets       map[string]*tokenBucket
	cfg           RateLimitConfig
	clock         func() time.Time
	trustedNets   []*net.IPNet
	trustedIPs    []net.IP
	allowedTotal  atomic.Uint64
	rejectedTotal atomic.Uint64
	stopCleanup   chan struct{}
	cleanupDone   chan struct{}
	stopped       atomic.Bool
}

// NewLimiter creates and validates an incoming rate limiter.
func NewLimiter(cfg RateLimitConfig) (*Limiter, error) {
	if math.IsNaN(cfg.RequestsPerSec) || math.IsInf(cfg.RequestsPerSec, 0) || cfg.RequestsPerSec <= 0 {
		cfg.RequestsPerSec = DefaultRequestsPerSecond
	}
	if cfg.BurstCapacity <= 0 {
		cfg.BurstCapacity = DefaultBurstCapacity
	}
	if cfg.BucketTTL <= 0 {
		cfg.BucketTTL = DefaultBucketTTL
	}
	if cfg.MaxBuckets <= 0 {
		cfg.MaxBuckets = DefaultMaxBuckets
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}

	var trustedNets []*net.IPNet
	var trustedIPs []net.IP
	for _, raw := range cfg.TrustedProxies {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "/") {
			_, ipNet, err := net.ParseCIDR(trimmed)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", trimmed, err)
			}
			trustedNets = append(trustedNets, ipNet)
		} else {
			ip := net.ParseIP(trimmed)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted proxy IP %q", trimmed)
			}
			trustedIPs = append(trustedIPs, ip)
		}
	}

	limiter := &Limiter{
		buckets:     make(map[string]*tokenBucket),
		cfg:         cfg,
		clock:       cfg.Clock,
		trustedNets: trustedNets,
		trustedIPs:  trustedIPs,
	}

	if cfg.CleanupInterval > 0 {
		limiter.stopCleanup = make(chan struct{})
		limiter.cleanupDone = make(chan struct{})
		go limiter.runCleanup(cfg.CleanupInterval)
	}

	return limiter, nil
}

// Allow determines whether a request under the specified client key is admitted.
func (l *Limiter) Allow(clientKey string) (bool, time.Duration) {
	now := l.clock()

	// Fast path: existing bucket read lock
	l.mu.RLock()
	bucket, exists := l.buckets[clientKey]
	l.mu.RUnlock()

	if exists {
		allowed, wait := bucket.allow(now)
		if allowed {
			l.allowedTotal.Add(1)
		} else {
			l.rejectedTotal.Add(1)
		}
		return allowed, wait
	}

	// Slow path: create new bucket under write lock
	l.mu.Lock()
	bucket, exists = l.buckets[clientKey]
	if !exists {
		if len(l.buckets) >= l.cfg.MaxBuckets {
			l.evictExpiredLocked(now)
		}
		if len(l.buckets) >= l.cfg.MaxBuckets {
			// Fail closed for unallocated keys when the bucket table is saturated with active clients.
			// This bounds memory without evicting active clients or resetting their rate limits.
			l.mu.Unlock()
			l.rejectedTotal.Add(1)
			waitSec := math.Ceil(1.0 / l.cfg.RequestsPerSec)
			if math.IsNaN(waitSec) || waitSec < 1.0 {
				waitSec = 1.0
			} else if waitSec > maxRetryAfterSeconds {
				waitSec = maxRetryAfterSeconds
			}
			return false, time.Duration(waitSec) * time.Second
		}

		// New bucket starts and consumes its first token immediately
		l.buckets[clientKey] = newTokenBucket(l.cfg.BurstCapacity, l.cfg.RequestsPerSec, now)
		l.mu.Unlock()
		l.allowedTotal.Add(1)
		return true, 0
	}
	l.mu.Unlock()

	// If another goroutine won the race to create it
	allowed, wait := bucket.allow(now)
	if allowed {
		l.allowedTotal.Add(1)
	} else {
		l.rejectedTotal.Add(1)
	}
	return allowed, wait
}

func (l *Limiter) evictExpiredLocked(now time.Time) int {
	evicted := 0
	for key, b := range l.buckets {
		b.mu.Lock()
		lastSeen := b.lastSeen
		b.mu.Unlock()

		if now.Sub(lastSeen) > l.cfg.BucketTTL {
			delete(l.buckets, key)
			evicted++
		}
	}
	return evicted
}

// Cleanup removes stale buckets that have been inactive longer than BucketTTL.
// Returns the number of evicted buckets.
func (l *Limiter) Cleanup(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.evictExpiredLocked(now)
}

func (l *Limiter) runCleanup(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.Cleanup(l.clock())
		case <-l.stopCleanup:
			close(l.cleanupDone)
			return
		}
	}
}

// Stop terminates the background cleanup goroutine if running.
func (l *Limiter) Stop() {
	if l.stopCleanup != nil && l.stopped.CompareAndSwap(false, true) {
		close(l.stopCleanup)
		<-l.cleanupDone
	}
}

// Metrics returns the current execution metrics for the rate limiter.
func (l *Limiter) Metrics() LimiterMetrics {
	allowed := l.allowedTotal.Load()
	rejected := l.rejectedTotal.Load()
	total := allowed + rejected
	var rate float64
	if total > 0 {
		rate = float64(rejected) / float64(total)
	}

	l.mu.RLock()
	active := len(l.buckets)
	l.mu.RUnlock()

	return LimiterMetrics{
		AllowedRequests:  allowed,
		RejectedRequests: rejected,
		ActiveBuckets:    active,
		RejectionRate:    rate,
	}
}

// isTrustedProxy checks if the given remote IP address matches configured trusted proxies.
func (l *Limiter) isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, trustedIP := range l.trustedIPs {
		if trustedIP.Equal(ip) {
			return true
		}
	}
	for _, trustedNet := range l.trustedNets {
		if trustedNet.Contains(ip) {
			return true
		}
	}
	return false
}

// ExtractClientKey securely identifies the client.
// Precedence:
// 1. Verified authenticated principal from request context (hashed), if auth middleware populated it.
// 2. Peer IP from RemoteAddr (with trusted proxy X-Forwarded-For traversal only if peer is a trusted proxy).
//
// Unverified Authorization, X-API-Key, X-Real-IP, and untrusted X-Forwarded-For headers are never trusted.
func (l *Limiter) ExtractClientKey(r *http.Request) string {
	if principal, ok := r.Context().Value(principalContextKey{}).(string); ok {
		trimmed := strings.TrimSpace(principal)
		if trimmed != "" {
			h := sha256.Sum256([]byte(trimmed))
			return "principal:" + hex.EncodeToString(h[:16])
		}
	}
	return l.ExtractPeerIPKey(r)
}

// ExtractPeerIPKey extracts only the trusted peer IP key, ignoring all auth headers,
// context principals, X-Real-IP, and untrusted X-Forwarded-For headers.
func (l *Limiter) ExtractPeerIPKey(r *http.Request) string {
	remoteHost := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remoteHost); err == nil {
		remoteHost = host
	} else {
		remoteHost = strings.TrimSuffix(strings.TrimPrefix(remoteHost, "["), "]")
	}
	remoteIP := net.ParseIP(remoteHost)

	// Only inspect X-Forwarded-For when the immediate socket peer is a configured trusted proxy.
	if remoteIP != nil && l.isTrustedProxy(remoteIP) {
		xffValues := r.Header.Values("X-Forwarded-For")
		if len(xffValues) > 0 {
			var parts []string
			for _, v := range xffValues {
				parts = append(parts, strings.Split(v, ",")...)
			}
			// Walk right-to-left: stop at the first untrusted valid IP, or break on malformed entries
			// so an attacker cannot inject an earlier IP behind a malformed token.
			for i := len(parts) - 1; i >= 0; i-- {
				candidate := strings.TrimSpace(parts[i])
				if candidate == "" {
					break
				}
				candIP := net.ParseIP(candidate)
				if candIP == nil {
					break
				}
				if !l.isTrustedProxy(candIP) {
					return "ip:" + candIP.String()
				}
			}
		}
	}

	if remoteIP != nil {
		return "ip:" + remoteIP.String()
	}
	if remoteHost == "" {
		return "ip:unknown"
	}
	return "ip:" + remoteHost
}

func isHealthProbePath(path string) bool {
	return path == "/health/live" || path == "/health/ready"
}

func writeRateLimitExceeded(w http.ResponseWriter, wait time.Duration) {
	retryAfterSec := int64(math.Ceil(wait.Seconds()))
	if retryAfterSec < 1 {
		retryAfterSec = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSec, 10))
	writeError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests, please slow down")
}

// PreAuthIPRateLimitMiddleware enforces Stage 1 rate limiting before credential verification
// using strictly the trusted peer IP so unauthenticated floods cannot exhaust DB connections.
func PreAuthIPRateLimitMiddleware(ipLimiter *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isHealthProbePath(r.URL.Path) || ipLimiter == nil || !ipLimiter.cfg.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			clientKey := ipLimiter.ExtractPeerIPKey(r)
			if allowed, wait := ipLimiter.Allow(clientKey); !allowed {
				writeRateLimitExceeded(w, wait)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AuthenticatedPrincipalRateLimitMiddleware enforces Stage 2 rate limiting after credential
// verification using the verified principal identity. Unauthenticated requests pass through
// without allocating principal buckets so RequireAuthMiddleware can return 401/503.
func AuthenticatedPrincipalRateLimitMiddleware(principalLimiter *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isHealthProbePath(r.URL.Path) || principalLimiter == nil || !principalLimiter.cfg.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			principal, ok := auth.PrincipalFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			h := sha256.Sum256([]byte(principal.TenantID + ":" + principal.PrincipalID))
			clientKey := "principal:" + hex.EncodeToString(h[:16])
			if allowed, wait := principalLimiter.Allow(clientKey); !allowed {
				writeRateLimitExceeded(w, wait)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimitMiddleware returns an HTTP middleware that enforces token-bucket rate limits.
// Excludes /health/live and /health/ready probes.
func RateLimitMiddleware(limiter *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isHealthProbePath(r.URL.Path) || limiter == nil || !limiter.cfg.Enabled {
				next.ServeHTTP(w, r)
				return
			}

			clientKey := limiter.ExtractClientKey(r)
			if allowed, wait := limiter.Allow(clientKey); !allowed {
				writeRateLimitExceeded(w, wait)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Config returns the current configuration of the limiter.
func (l *Limiter) Config() RateLimitConfig {
	return l.cfg
}

// Reset clears all active client buckets (primarily for testing).
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = make(map[string]*tokenBucket)
	l.allowedTotal.Store(0)
	l.rejectedTotal.Store(0)
}

// ErrInvalidConfig is returned when rate limiter configuration is unparseable or contradictory.
var ErrInvalidConfig = errors.New("invalid rate limiter configuration")

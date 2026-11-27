package llmagent

import (
	"context"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// AttemptDiagnostic records observability telemetry for each individual model request attempt.
type AttemptDiagnostic struct {
	Attempt      int    `json:"attempt"`
	StatusCode   int    `json:"status_code,omitempty"`
	Category     string `json:"category,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
	RetryDelayMs int64  `json:"retry_delay_ms,omitempty"`
	Retryable    *bool  `json:"retryable,omitempty"`
	Error        string `json:"error,omitempty"`
}

// RetryPolicy defines the bounded exponential backoff parameters with full jitter.
type RetryPolicy struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	RNG          func() float64
	Sleeper      func(ctx context.Context, d time.Duration) error
}

var (
	defaultRNGMu sync.Mutex
	defaultRNG   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func defaultRandomFloat() float64 {
	defaultRNGMu.Lock()
	defer defaultRNGMu.Unlock()
	return defaultRNG.Float64()
}

func defaultSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DefaultRetryPolicy returns a conservative retry policy tuned for cloud LLM rate limits.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:  4, // 1 initial attempt + up to 3 retries
		InitialDelay: 500 * time.Millisecond,
		MaxDelay:     10 * time.Second,
		RNG:          defaultRandomFloat,
		Sleeper:      defaultSleep,
	}
}

// ComputeDelay calculates the backoff delay using the full-jitter formula:
// delay = random(0, min(MaxDelay, InitialDelay * 2^(attempt-1)))
// If serverRetryAfter > 0, the delay honors the server's requested delay up to MaxDelay.
func (p *RetryPolicy) ComputeDelay(attempt int, serverRetryAfter time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	initial := p.InitialDelay
	if initial <= 0 {
		initial = 500 * time.Millisecond
	}
	maxD := p.MaxDelay
	if maxD <= 0 {
		maxD = 10 * time.Second
	}

	// Exponential factor with overflow protection
	shift := attempt - 1
	if shift > 30 {
		shift = 30
	}
	factor := math.Pow(2, float64(shift))
	calculated := float64(initial) * factor
	if calculated > float64(maxD) || math.IsInf(calculated, 0) {
		calculated = float64(maxD)
	}

	rng := p.RNG
	if rng == nil {
		rng = defaultRandomFloat
	}

	// Full jitter: uniformly random between 0 and calculated ceiling
	jittered := time.Duration(rng() * calculated)

	// If server specified a Retry-After header, ensure we don't fire earlier than requested,
	// but clamp to MaxDelay so malicious/buggy servers cannot hang execution indefinitely.
	if serverRetryAfter > 0 {
		if serverRetryAfter > maxD {
			serverRetryAfter = maxD
		}
		if serverRetryAfter > jittered {
			jittered = serverRetryAfter
		}
	}

	if jittered > maxD {
		jittered = maxD
	}
	if jittered < 0 {
		jittered = 0
	}
	return jittered
}

// IsRetryable determines whether an HTTP status code or error qualifies for bounded retry.
func IsRetryable(statusCode int, err error) bool {
	if statusCode == http.StatusTooManyRequests || statusCode == http.StatusServiceUnavailable {
		return true
	}
	// Note: 400, 401, 403, 404 and other client errors are strictly non-retryable.
	// Network timeouts or connection resets may be handled if classified as transient,
	// but context cancellation must NEVER be retried.
	return false
}

// ParseRetryAfter parses HTTP Retry-After headers in either delta-seconds or RFC1123/RFC850 formats.
func ParseRetryAfter(header string, now time.Time) time.Duration {
	if header == "" {
		return 0
	}
	// 1. Try delta-seconds integer
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	// 2. Try HTTP-date format (RFC1123 / RFC850 / ANSIC)
	formats := []string{
		http.TimeFormat, // RFC1123
		time.RFC850,
		time.ANSIC,
	}
	for _, format := range formats {
		if t, err := time.Parse(format, header); err == nil {
			diff := t.Sub(now)
			if diff <= 0 {
				return 0
			}
			return diff
		}
	}
	return 0
}

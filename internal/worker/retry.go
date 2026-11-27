package worker

import (
	"errors"
	"math"
	"math/rand/v2"
	"time"
)

// Default retry policy values.
const (
	DefaultMaxAttempts = 5
	DefaultBaseDelay   = 2 * time.Second
	DefaultMaxDelay    = 5 * time.Minute
)

// NonRetryableError marks an error as permanent (e.g. validation or authorization failure),
// causing immediate transition to dead_letter without further retries.
type NonRetryableError struct {
	Err error
}

func (e *NonRetryableError) Error() string {
	if e == nil || e.Err == nil {
		return "non-retryable error"
	}
	return e.Err.Error()
}

func (e *NonRetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// MarkNonRetryable wraps an error so the worker engine transitions the job to dead_letter immediately.
func MarkNonRetryable(err error) error {
	if err == nil {
		return nil
	}
	return &NonRetryableError{Err: err}
}

// IsRetryable reports whether err should be retried under the retry policy.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var nonRetryable *NonRetryableError
	return !errors.As(err, &nonRetryable)
}

// RetryPolicy configures bounded exponential backoff with full jitter.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	RandFloat64 func() float64
}

// NextDelay computes a bounded exponential backoff with full jitter for the given attempt (1-indexed).
func (p RetryPolicy) NextDelay(attempt int) time.Duration {
	base := p.BaseDelay
	if base <= 0 {
		base = DefaultBaseDelay
	}
	maxDelay := p.MaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultMaxDelay
	}
	if maxDelay < base {
		maxDelay = base
	}
	if attempt < 1 {
		attempt = 1
	}

	exp := float64(attempt - 1)
	if exp > 30 {
		exp = 30
	}
	raw := float64(base) * math.Pow(2, exp)
	capped := math.Min(raw, float64(maxDelay))

	randFn := p.RandFloat64
	if randFn == nil {
		randFn = rand.Float64
	}
	jitter := randFn()
	if math.IsNaN(jitter) || jitter < 0 {
		jitter = 0
	} else if jitter > 1 {
		jitter = 1
	}

	// Half-to-full jitter [0.5*capped, capped] keeps retries spread out without zero-delay tight loops.
	delay := time.Duration(capped * (0.5 + 0.5*jitter))
	if delay < time.Millisecond {
		delay = time.Millisecond
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

package upstream

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrQuotaReserve is matched by errors.Is when the remaining quota is at or
	// below the reserve threshold, so only cached data may be served.
	ErrQuotaReserve = errors.New("gitprovider: quota below reserve threshold")

	// ErrMaxRetry is matched by errors.Is when the request kept failing with a
	// retryable error until the retry budget was spent.
	ErrMaxRetry = errors.New("gitprovider: max retry reached")

	// ErrUnauthorized is matched by errors.Is when the provider answered 401.
	ErrUnauthorized = errors.New("gitprovider: unauthorized")
)

// QuotaReserveError reports why a request was not sent.
// It matches ErrQuotaReserve with errors.Is.
type QuotaReserveError struct {
	// Remaining is the last known remaining quota.
	Remaining int
	// Reserve is the threshold that was applied.
	Reserve int
	// ResetAt is when the provider resets the quota window.
	ResetAt time.Time
}

// Error implements error.
func (e *QuotaReserveError) Error() string {
	return fmt.Sprintf("%s: remaining %d, reserve %d, resets at %s",
		ErrQuotaReserve, e.Remaining, e.Reserve, e.ResetAt.UTC().Format(time.RFC3339))
}

// Is reports whether target is ErrQuotaReserve.
func (e *QuotaReserveError) Is(target error) bool { return target == ErrQuotaReserve }

// MaxRetryError wraps the last failure of a request that exhausted its retries.
// It matches ErrMaxRetry with errors.Is and unwraps to the last cause.
type MaxRetryError struct {
	// Attempts is the number of requests that were sent.
	Attempts int
	// Err is the cause of the last attempt.
	Err error
}

// Error implements error.
func (e *MaxRetryError) Error() string {
	return fmt.Sprintf("%s after %d attempt(s): %v", ErrMaxRetry, e.Attempts, e.Err)
}

// Is reports whether target is ErrMaxRetry.
func (e *MaxRetryError) Is(target error) bool { return target == ErrMaxRetry }

// Unwrap returns the last cause.
func (e *MaxRetryError) Unwrap() error { return e.Err }

// StatusError describes an unsuccessful HTTP status returned by the provider.
// A 401 status additionally matches ErrUnauthorized.
type StatusError struct {
	StatusCode int
	// RetryAfter is the wait requested by the provider, zero when absent.
	RetryAfter time.Duration
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("gitprovider: unexpected status %d", e.StatusCode)
}

// Is reports whether target is ErrUnauthorized and the status is 401.
func (e *StatusError) Is(target error) bool {
	return target == ErrUnauthorized && e.StatusCode == 401
}

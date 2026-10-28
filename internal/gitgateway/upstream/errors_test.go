package upstream_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/upstream"
)

func TestErrorsMatchSentinels(t *testing.T) {
	cause := &upstream.StatusError{StatusCode: 503}
	tests := []struct {
		name string
		err  error
		is   error
		not  error
	}{
		{"quota", &upstream.QuotaReserveError{Remaining: 3, Reserve: 10}, upstream.ErrQuotaReserve, upstream.ErrMaxRetry},
		{"max retry", &upstream.MaxRetryError{Attempts: 3, Err: cause}, upstream.ErrMaxRetry, upstream.ErrUnauthorized},
		{"unauthorized", &upstream.StatusError{StatusCode: 401}, upstream.ErrUnauthorized, upstream.ErrMaxRetry},
		{"not found is not unauthorized", &upstream.StatusError{StatusCode: 404}, nil, upstream.ErrUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.is != nil && !errors.Is(tt.err, tt.is) {
				t.Errorf("errors.Is(%v, %v) = false", tt.err, tt.is)
			}
			if errors.Is(tt.err, tt.not) {
				t.Errorf("errors.Is(%v, %v) = true", tt.err, tt.not)
			}
		})
	}
}

func TestMaxRetryErrorUnwrapsLastCause(t *testing.T) {
	err := error(&upstream.MaxRetryError{Attempts: 3, Err: &upstream.StatusError{StatusCode: 502}})
	var status *upstream.StatusError
	if !errors.As(err, &status) || status.StatusCode != 502 {
		t.Fatalf("errors.As did not find the last cause: %v", err)
	}
	if !strings.Contains(err.Error(), "3 attempt") {
		t.Errorf("message %q lacks the attempt count", err)
	}
}

func TestQuotaReserveErrorMessage(t *testing.T) {
	err := &upstream.QuotaReserveError{Remaining: 4, Reserve: 10, ResetAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	if !strings.Contains(err.Error(), "2026-01-02T03:04:05Z") {
		t.Errorf("message %q lacks the reset time", err)
	}
}

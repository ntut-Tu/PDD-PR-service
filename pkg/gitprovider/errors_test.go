package gitprovider_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"pdd/services/pkg/gitprovider"
)

func TestErrorsMatchSentinels(t *testing.T) {
	cause := &gitprovider.StatusError{StatusCode: 503}
	tests := []struct {
		name string
		err  error
		is   error
		not  error
	}{
		{"quota", &gitprovider.QuotaReserveError{Remaining: 3, Reserve: 10}, gitprovider.ErrQuotaReserve, gitprovider.ErrMaxRetry},
		{"max retry", &gitprovider.MaxRetryError{Attempts: 3, Err: cause}, gitprovider.ErrMaxRetry, gitprovider.ErrUnauthorized},
		{"unauthorized", &gitprovider.StatusError{StatusCode: 401}, gitprovider.ErrUnauthorized, gitprovider.ErrMaxRetry},
		{"not found is not unauthorized", &gitprovider.StatusError{StatusCode: 404}, nil, gitprovider.ErrUnauthorized},
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
	err := error(&gitprovider.MaxRetryError{Attempts: 3, Err: &gitprovider.StatusError{StatusCode: 502}})
	var status *gitprovider.StatusError
	if !errors.As(err, &status) || status.StatusCode != 502 {
		t.Fatalf("errors.As did not find the last cause: %v", err)
	}
	if !strings.Contains(err.Error(), "3 attempt") {
		t.Errorf("message %q lacks the attempt count", err)
	}
}

func TestQuotaReserveErrorMessage(t *testing.T) {
	err := &gitprovider.QuotaReserveError{Remaining: 4, Reserve: 10, ResetAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	if !strings.Contains(err.Error(), "2026-01-02T03:04:05Z") {
		t.Errorf("message %q lacks the reset time", err)
	}
}

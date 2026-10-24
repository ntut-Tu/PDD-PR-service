package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"pdd/services/internal/gitgateway/auth"
	"pdd/services/internal/gitgateway/auth/authtest"
)

const secret = "48521562@2f65r6@555&&^#"

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func verifier() *auth.Verifier {
	return auth.NewVerifier(secret, func() time.Time { return now })
}

func TestVerifyValidLegacyToken(t *testing.T) {
	token := authtest.Sign(secret, "alice", "gho_oauth", now.Add(10*time.Minute))

	claims, err := verifier().Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Account != "alice" || claims.ProviderToken != "gho_oauth" {
		t.Fatalf("claims = %+v", claims)
	}
}

func TestVerifyRejects(t *testing.T) {
	noneToken, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "alice", "oauth": "x", "exp": now.Add(time.Minute).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	noExp, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "alice", "oauth": "x"}).SignedString([]byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	valid := now.Add(time.Minute)
	tests := map[string]string{
		"bad signature":   authtest.Sign("another-secret", "alice", "gho", valid),
		"expired":         authtest.Sign(secret, "alice", "gho", now.Add(-time.Second)),
		"missing sub":     authtest.Sign(secret, "", "gho", valid),
		"missing oauth":   authtest.Sign(secret, "alice", "", valid),
		"alg none":        noneToken,
		"missing expiry":  noExp,
		"not a jwt":       "garbage",
		"empty":           "",
		"raw secret used": rawKeyToken(t, valid),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier().Verify(token); !errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("Verify error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

// rawKeyToken is signed with the secret itself instead of its base64url text.
func rawKeyToken(t *testing.T, exp time.Time) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "alice", "oauth": "x", "exp": exp.Unix(),
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"Bearer  abc ", "abc", true},
		{"Basic abc", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := auth.BearerToken(tt.header)
		if got != tt.want || ok != tt.ok {
			t.Errorf("BearerToken(%q) = %q, %v; want %q, %v", tt.header, got, ok, tt.want, tt.ok)
		}
	}
}

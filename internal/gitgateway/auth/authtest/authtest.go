// Package authtest signs PDD login tokens the way the legacy backend does, for tests.
package authtest

import (
	"encoding/base64"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Sign returns an HS256 token with the claims of the legacy JwtHelper. The key
// is the base64url text of secret, used as raw bytes.
func Sign(secret, account, oauth string, expires time.Time) string {
	key := []byte(base64.URLEncoding.EncodeToString([]byte(secret)))
	claims := jwt.MapClaims{
		"jti": "test-id", "iat": expires.Add(-10 * time.Minute).Unix(), "sub": account,
		"oauth": oauth, "roles": "USER", "iss": "JwtAuth", "exp": expires.Unix(),
	}
	for name, value := range map[string]any{"sub": account, "oauth": oauth} {
		if value == "" {
			delete(claims, name)
		}
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		panic(err)
	}
	return signed
}

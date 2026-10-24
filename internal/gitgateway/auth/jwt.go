// Package auth verifies the PDD login JWT.
package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken is matched by errors.Is for every rejected token.
var ErrInvalidToken = errors.New("auth: invalid token")

// Claims are the identity fields the gateway uses from a PDD JWT.
type Claims struct {
	// Account is the PDD account (the sub claim).
	Account string
	// ProviderToken is the GitHub or GitLab OAuth access token (the oauth claim).
	ProviderToken string
}

type tokenClaims struct {
	jwt.RegisteredClaims
	OAuth string `json:"oauth"`
}

// Verifier validates HS256 tokens signed by the PDD backend.
type Verifier struct {
	key    []byte
	parser *jwt.Parser
}

// NewVerifier creates a Verifier for the backend secret. The legacy backend
// signs with the base64url text of the secret used as raw key bytes, so the
// same derivation is applied here. now may be nil to use the system clock.
func NewVerifier(secret string, now func() time.Time) *Verifier {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	}
	if now != nil {
		opts = append(opts, jwt.WithTimeFunc(now))
	}
	return &Verifier{
		key:    []byte(base64.URLEncoding.EncodeToString([]byte(secret))),
		parser: jwt.NewParser(opts...),
	}
}

// Verify checks the signature and expiry of token and returns its claims.
func (v *Verifier) Verify(token string) (Claims, error) {
	var claims tokenClaims
	_, err := v.parser.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return v.key, nil })
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if claims.Subject == "" || claims.OAuth == "" {
		return Claims{}, fmt.Errorf("%w: missing sub or oauth claim", ErrInvalidToken)
	}
	return Claims{Account: claims.Subject, ProviderToken: claims.OAuth}, nil
}

// BearerToken extracts the token of an "Authorization: Bearer <token>" header.
func BearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

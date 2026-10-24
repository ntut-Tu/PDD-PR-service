package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrTokenNotFound is matched by errors.Is when no token is stored for an account.
var ErrTokenNotFound = errors.New("store: no token stored for account")

// TokenStore keeps the provider OAuth token of each PDD account, encrypted at rest.
type TokenStore struct {
	db     *sql.DB
	cipher *Cipher
	now    func() time.Time
}

// NewTokenStore creates a TokenStore. A nil now defaults to time.Now.
func NewTokenStore(db *sql.DB, cipher *Cipher, now func() time.Time) *TokenStore {
	if now == nil {
		now = time.Now
	}
	return &TokenStore{db: db, cipher: cipher, now: now}
}

// Save stores token for account, replacing the previous one.
func (s *TokenStore) Save(ctx context.Context, account, token string) error {
	sealed, err := s.cipher.Seal([]byte(token), []byte(account))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO provider_tokens (account, ciphertext, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(account) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		account, sealed, s.now().UnixNano())
	if err != nil {
		return fmt.Errorf("store: save token: %w", err)
	}
	return nil
}

// Load returns the token stored for account, or ErrTokenNotFound.
func (s *TokenStore) Load(ctx context.Context, account string) (string, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM provider_tokens WHERE account = ?`, account).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTokenNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: load token: %w", err)
	}
	plaintext, err := s.cipher.Open(sealed, []byte(account))
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

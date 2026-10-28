package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// ErrDecrypt is matched by errors.Is when a ciphertext cannot be opened, for
// example after the key changed.
var ErrDecrypt = errors.New("store: cannot decrypt")

// Cipher encrypts small secrets with AES-256-GCM. The output is the random
// nonce followed by the sealed data.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher creates a Cipher from a 32-byte key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("store: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: create gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Seal encrypts plaintext. aad is authenticated but not stored; Open needs the
// same value, which binds a ciphertext to the record it belongs to.
func (c *Cipher) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("store: read random nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts the output of Seal.
func (c *Cipher) Open(sealed, aad []byte) ([]byte, error) {
	size := c.aead.NonceSize()
	if len(sealed) < size {
		return nil, ErrDecrypt
	}
	plaintext, err := c.aead.Open(nil, sealed[:size], sealed[size:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

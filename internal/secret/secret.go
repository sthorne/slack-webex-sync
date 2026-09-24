// Package secret encrypts small values (OAuth tokens) before they are
// stored, using AES-256-GCM.
//
// Encrypted values are text: "enc:v1:" followed by base64 of nonce and
// ciphertext. Anything without that prefix is treated as plain text, so a
// store written before encryption was turned on still loads, and is
// re-encrypted on the next save.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const prefix = "enc:v1:"

// ErrNoKey means a value is encrypted but no key was configured.
var ErrNoKey = errors.New("value is encrypted but no storage.encryption_key is configured")

// Box encrypts and decrypts values. A nil *Box stores values in plain text.
type Box struct {
	aead cipher.AEAD
}

// GenerateKey returns a new random key, base64 encoded.
func GenerateKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// New builds a Box from a base64 key of 32 bytes. An empty key returns
// (nil, nil): no encryption.
func New(encodedKey string) (*Box, error) {
	encodedKey = strings.TrimSpace(encodedKey)
	if encodedKey == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("encryption key is not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d (generate one with `slack-webex-sync generate-key`)", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Enabled reports whether values are encrypted.
func (b *Box) Enabled() bool { return b != nil }

// IsEncrypted reports whether a stored value is in encrypted form.
func IsEncrypted(value string) bool { return strings.HasPrefix(value, prefix) }

// Seal encrypts plaintext. With a nil Box it returns plaintext unchanged.
// label is bound to the ciphertext, so a value copied to another key fails
// to decrypt.
func (b *Box) Seal(plaintext, label string) (string, error) {
	if b == nil {
		return plaintext, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), []byte(label))
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a stored value. Plain-text values are returned unchanged.
func (b *Box) Open(value, label string) (string, error) {
	if !IsEncrypted(value) {
		return value, nil
	}
	if b == nil {
		return "", ErrNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("decrypt: value too short")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(label))
	if err != nil {
		return "", errors.New("decrypt failed: wrong storage.encryption_key, or the value was altered")
	}
	return string(plain), nil
}

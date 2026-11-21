// Package secrets encrypts sensitive values (SSH private keys, repository
// passphrases, identity provider secrets) before they are stored in SQLite.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "enc:v1:"

// Box encrypts and decrypts values with AES-256-GCM.
type Box struct {
	aead cipher.AEAD
}

// Open returns a Box for the base64 key in keyB64. When keyB64 is empty the
// key is read from keyFile, which is created with a random key if missing.
func Open(keyB64, keyFile string) (*Box, error) {
	if keyB64 == "" {
		data, err := os.ReadFile(keyFile)
		switch {
		case errors.Is(err, os.ErrNotExist):
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			keyB64 = base64.StdEncoding.EncodeToString(key)
			if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
				return nil, err
			}
			if err := os.WriteFile(keyFile, []byte(keyB64+"\n"), 0o600); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		default:
			keyB64 = strings.TrimSpace(string(data))
		}
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, base64 encoded")
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

// Encrypt returns the encrypted form of plain; an empty value stays empty.
func (b *Box) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt.
func (b *Box) Decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, prefix) {
		return "", errors.New("value is not encrypted")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("encrypted value too short")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt value (wrong secret key?)")
	}
	return string(plain), nil
}

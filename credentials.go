package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Monit includes the user and password of its httpd in every report (unless
// monitrc says "register without credentials"). They are stored so that the
// web interface can send actions (start, stop, restart, monitor, unmonitor)
// to the agent, like M/Monit does.
//
// Stored format of hosts.httpd_password:
//
//	""                         no credentials in the report
//	"plain:<password>"         no key configured
//	"aes256gcm:<base64>"       base64(nonce[12] | ciphertext | tag[16]),
//	                           AES-256-GCM with the host's monit_id as
//	                           additional data, key from BMON_CREDENTIALS_KEY
//
// Bmon-web decrypts with the same key (credentials_key in its config).

const (
	credPlainPrefix = "plain:"
	credAESPrefix   = "aes256gcm:"
	maxPasswordLen  = 512
)

type credentialSealer struct {
	aead cipher.AEAD // nil: passwords are stored in clear
}

// newCredentialSealer accepts an empty key (no encryption) or 32 bytes in base64.
func newCredentialSealer(b64Key string) (*credentialSealer, error) {
	b64Key = strings.TrimSpace(b64Key)
	if b64Key == "" {
		return &credentialSealer{}, nil
	}
	key, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil {
		return nil, fmt.Errorf("credentials key: not valid base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("credentials key: must be 32 bytes, got %d (generate one with: openssl rand -base64 32)", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &credentialSealer{aead: aead}, nil
}

func (c *credentialSealer) Encrypted() bool { return c != nil && c.aead != nil }

// Seal returns the value to store for a password ("" for no password).
func (c *credentialSealer) Seal(password, monitID string) (string, error) {
	if password == "" {
		return "", nil
	}
	if len(password) > maxPasswordLen {
		return "", errors.New("httpd password too long")
	}
	if !c.Encrypted() {
		return credPlainPrefix + password, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := c.aead.Seal(nonce, nonce, []byte(password), []byte(monitID))
	return credAESPrefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open reverses Seal (used by the tests; Bmon-web has its own implementation).
func (c *credentialSealer) Open(stored, monitID string) (string, error) {
	switch {
	case stored == "":
		return "", nil
	case strings.HasPrefix(stored, credPlainPrefix):
		return strings.TrimPrefix(stored, credPlainPrefix), nil
	case strings.HasPrefix(stored, credAESPrefix):
		if !c.Encrypted() {
			return "", errors.New("password is encrypted but no key is configured")
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, credAESPrefix))
		if err != nil || len(raw) < c.aead.NonceSize()+c.aead.Overhead() {
			return "", errors.New("malformed encrypted password")
		}
		n := c.aead.NonceSize()
		pt, err := c.aead.Open(nil, raw[:n], raw[n:], []byte(monitID))
		if err != nil {
			return "", errors.New("cannot decrypt password (wrong key?)")
		}
		return string(pt), nil
	}
	return "", errors.New("unknown password format")
}

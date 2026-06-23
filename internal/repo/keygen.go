package repo

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/base64"
)

// apiKeyPrefix is the human-readable prefix on generated keys.
const apiKeyPrefix = "ak"

// hashKey returns the SHA-256 hex digest of a raw key (mirrors hashKey).
func hashKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

// createRawKey generates "ak-" + 24 random bytes base64url (mirrors createRawKey).
func createRawKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return apiKeyPrefix + "-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// createKeyID generates 16 random bytes as hex (mirrors createKeyId).
func createKeyID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// keyPrefix returns the first 10 chars of rawKey (mirrors rawKey.slice(0,10)).
func keyPrefix(rawKey string) string {
	if len(rawKey) < 10 {
		return rawKey
	}
	return rawKey[:10]
}

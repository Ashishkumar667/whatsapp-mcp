package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// mediaEncryptionKey, if set, is used to encrypt the media decryption keys
// (media_key/file_sha256/file_enc_sha256) before they're stored in MongoDB -
// those three fields together are effectively "the keys to unlock someone's
// photos/videos", so they're worth protecting even if the DB is only ever
// read by this server. Optional: if unset, these fields are stored in plain
// bytes as before, so existing deployments aren't broken by adding this.
var mediaEncryptionKey []byte

// SetMediaEncryptionKey configures the AES-256-GCM key used by encryptBytes/decryptBytes,
// from a 64-character hex string (32 bytes). Call once at startup.
func SetMediaEncryptionKey(hexKey string) error {
	if hexKey == "" {
		mediaEncryptionKey = nil
		return nil
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return fmt.Errorf("MEDIA_ENCRYPTION_KEY must be hex-encoded: %w", err)
	}
	if len(key) != 32 {
		return fmt.Errorf("MEDIA_ENCRYPTION_KEY must decode to 32 bytes (64 hex chars), got %d", len(key))
	}
	mediaEncryptionKey = key
	return nil
}

func mediaEncryptionEnabled() bool {
	return len(mediaEncryptionKey) == 32
}

// encryptBytes encrypts plaintext with AES-256-GCM, returning nonce||ciphertext.
// Returns plaintext unchanged if no key is configured.
func encryptBytes(plaintext []byte) ([]byte, error) {
	if !mediaEncryptionEnabled() || len(plaintext) == 0 {
		return plaintext, nil
	}
	block, err := aes.NewCipher(mediaEncryptionKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decryptBytes reverses encryptBytes. Returns input unchanged if no key is configured,
// and also falls back to returning the input unchanged if it doesn't decrypt as valid
// ciphertext under the current key - that's treated as legacy plaintext data stored
// before MEDIA_ENCRYPTION_KEY was turned on, rather than an error, so enabling
// encryption doesn't break downloads for messages that synced before the switch.
func decryptBytes(data []byte) ([]byte, error) {
	if !mediaEncryptionEnabled() || len(data) == 0 {
		return data, nil
	}
	block, err := aes.NewCipher(mediaEncryptionKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return data, nil
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return data, nil
	}
	return plaintext, nil
}

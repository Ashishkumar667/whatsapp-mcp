package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	if err := SetMediaEncryptionKey(strings.Repeat("0", 64)); err != nil {
		t.Fatalf("SetMediaEncryptionKey: %v", err)
	}
	defer SetMediaEncryptionKey("")

	plaintext := []byte("super-secret-media-key-bytes")
	ciphertext, err := encryptBytes(plaintext)
	if err != nil {
		t.Fatalf("encryptBytes: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext should not equal plaintext when a key is configured")
	}

	decrypted, err := decryptBytes(ciphertext)
	if err != nil {
		t.Fatalf("decryptBytes: %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("round trip mismatch: got %q want %q", decrypted, plaintext)
	}
}

func TestEncryptionDisabledByDefault(t *testing.T) {
	if err := SetMediaEncryptionKey(""); err != nil {
		t.Fatalf("SetMediaEncryptionKey: %v", err)
	}
	plaintext := []byte("plain-bytes")
	out, err := encryptBytes(plaintext)
	if err != nil {
		t.Fatalf("encryptBytes: %v", err)
	}
	if !bytes.Equal(out, plaintext) {
		t.Fatal("expected passthrough when no key is configured")
	}
}

func TestDecryptFallsBackForLegacyPlaintext(t *testing.T) {
	legacyPlaintext := []byte("stored-before-encryption-was-enabled")

	if err := SetMediaEncryptionKey(strings.Repeat("1", 64)); err != nil {
		t.Fatalf("SetMediaEncryptionKey: %v", err)
	}
	defer SetMediaEncryptionKey("")

	out, err := decryptBytes(legacyPlaintext)
	if err != nil {
		t.Fatalf("decryptBytes should not error on legacy plaintext, got: %v", err)
	}
	if !bytes.Equal(out, legacyPlaintext) {
		t.Fatalf("expected legacy plaintext to pass through unchanged, got %q", out)
	}
}

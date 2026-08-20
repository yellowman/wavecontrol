package secrets

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	m, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRoundTrip(t *testing.T) {
	m := testManager(t)
	enc, err := m.Encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, prefix) || enc == "secret" {
		t.Fatalf("unexpected ciphertext %q", enc)
	}
	got, err := m.Decrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("got %q", got)
	}
}

func TestPlaintextCompatibility(t *testing.T) {
	m := testManager(t)
	got, err := m.Decrypt("legacy")
	if err != nil || got != "legacy" {
		t.Fatalf("got %q err=%v", got, err)
	}
}

func TestRejectsWrongKeyLength(t *testing.T) {
	if _, err := New(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestEncryptTreatsCiphertextPrefixAsLiteralPlaintext(t *testing.T) {
	m := testManager(t)
	literal := prefix + "this-is-a-password"
	enc, err := m.Encrypt(literal)
	if err != nil {
		t.Fatal(err)
	}
	if enc == literal {
		t.Fatal("literal plaintext with encryption prefix was not encrypted")
	}
	got, err := m.Decrypt(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != literal {
		t.Fatalf("got %q, want %q", got, literal)
	}
}

func TestMigrationValidationRejectsMalformedCiphertext(t *testing.T) {
	m := testManager(t)
	if _, err := m.encryptForMigration(prefix + "not-valid-ciphertext"); err == nil {
		t.Fatal("expected malformed ciphertext to be rejected")
	}
}

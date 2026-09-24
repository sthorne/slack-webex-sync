package secret

import (
	"errors"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	box, err := New(key)
	if err != nil || !box.Enabled() {
		t.Fatalf("New = %v, %v", box, err)
	}
	sealed, err := box.Seal(`{"refresh_token":"r1"}`, "webex.tokens")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(sealed) || strings.Contains(sealed, "r1") {
		t.Fatalf("not encrypted: %q", sealed)
	}
	again, _ := box.Seal(`{"refresh_token":"r1"}`, "webex.tokens")
	if again == sealed {
		t.Error("nonce reused: identical ciphertexts")
	}
	plain, err := box.Open(sealed, "webex.tokens")
	if err != nil || plain != `{"refresh_token":"r1"}` {
		t.Fatalf("Open = %q, %v", plain, err)
	}
	if _, err := box.Open(sealed, "other.key"); err == nil {
		t.Error("value moved to another label decrypted")
	}
}

func TestWrongKeyAndTampering(t *testing.T) {
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	b1, _ := New(k1)
	b2, _ := New(k2)
	sealed, _ := b1.Seal("secret", "l")
	if _, err := b2.Open(sealed, "l"); err == nil {
		t.Error("decrypted with the wrong key")
	}
	tampered := sealed[:len(sealed)-4] + "AAAA"
	if _, err := b1.Open(tampered, "l"); err == nil {
		t.Error("tampered value decrypted")
	}
}

func TestPlainTextPassesThrough(t *testing.T) {
	var none *Box
	if v, _ := none.Seal("plain", "l"); v != "plain" {
		t.Errorf("nil box Seal = %q", v)
	}
	k, _ := GenerateKey()
	box, _ := New(k)
	if v, err := box.Open("legacy plain", "l"); err != nil || v != "legacy plain" {
		t.Errorf("plain value = %q, %v", v, err)
	}
	sealed, _ := box.Seal("x", "l")
	if _, err := none.Open(sealed, "l"); !errors.Is(err, ErrNoKey) {
		t.Errorf("err = %v, want ErrNoKey", err)
	}
}

func TestBadKeys(t *testing.T) {
	if b, err := New(""); b != nil || err != nil {
		t.Errorf("empty key = %v, %v", b, err)
	}
	if _, err := New("not base64!"); err == nil {
		t.Error("accepted invalid base64")
	}
	if _, err := New("c2hvcnQ="); err == nil {
		t.Error("accepted a short key")
	}
}

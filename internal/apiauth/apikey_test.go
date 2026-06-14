package apiauth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGenerateParseAndHashAPIKey(t *testing.T) {
	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Value, KeyPrefix+"_") {
		t.Fatalf("key prefix = %q", key.Value)
	}
	if strings.Contains(key.ID, "_") || strings.Contains(key.Secret, "_") {
		t.Fatalf("key components must not contain delimiter: id=%q secret=%q", key.ID, key.Secret)
	}
	if got := strings.Count(key.Value, "_"); got != 2 {
		t.Fatalf("key delimiter count = %d, want 2 in %q", got, key.Value)
	}
	parsed, err := ParseAPIKey(key.Value)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID != key.ID || parsed.Secret != key.Secret {
		t.Fatalf("parsed key = %#v, want %#v", parsed, key)
	}
	hash := HashAPIKey(key.Value)
	if !APIKeyHashMatches(key.Value, hash) {
		t.Fatal("hash should match original key")
	}
	if APIKeyHashMatches(key.Value+"x", hash) {
		t.Fatal("hash should not match modified key")
	}
}

func TestDeriveSubAccountAddr(t *testing.T) {
	got, err := DeriveSubAccountAddr("@alice@example.com", "bot-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "@alice_bot-1@example.com" {
		t.Fatalf("addr = %q", got)
	}
	if _, err := DeriveSubAccountAddr("@alice@example.com", "bad_agent"); err == nil {
		t.Fatal("underscore agent should fail")
	}
}

func TestParseEd25519PrivateKeyAcceptsSeed(t *testing.T) {
	seed := make([]byte, 32)
	encoded := base64.StdEncoding.EncodeToString(seed)
	key, err := ParseEd25519PrivateKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 64 {
		t.Fatalf("private key length = %d", len(key))
	}
}

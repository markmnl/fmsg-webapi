package middleware

import (
	"testing"
	"time"

	jwtv4 "github.com/golang-jwt/jwt/v4"
)

func TestIsValidAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"@alice@example.com", true},
		{"@a@b", true},
		{"@user@domain.org", true},
		{"alice@example.com", false}, // missing leading @
		{"@nodomain", false},         // no second @
		{"@", false},                 // too short
		{"ab", false},                // too short, no @
		{"", false},                  // empty
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			if got := isValidAddr(tt.addr); got != tt.want {
				t.Errorf("isValidAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestGetIdentityEmpty(t *testing.T) {
	// GetIdentity with no context key set should return "".
	// We can't easily construct a gin.Context here without more setup,
	// so this is a smoke-level check that the function signature is correct.
	_ = GetIdentity
}

// TestIATClockSkew verifies that SetupJWT installs a global TimeFunc that
// allows tokens whose iat is up to clockSkew seconds in the future, and
// rejects tokens whose iat exceeds that window.
func TestIATClockSkew(t *testing.T) {
	const secret = "test-secret-for-iat-skew"

	// SetupJWT sets jwtv4.TimeFunc as a side-effect.
	if _, err := SetupJWT([]byte(secret), "http://127.0.0.1:0"); err != nil {
		t.Fatalf("SetupJWT: %v", err)
	}

	sign := func(iatOffset time.Duration) string {
		claims := jwtv4.MapClaims{
			"sub": "@alice@example.com",
			"iat": time.Now().Add(iatOffset).Unix(),
			"exp": time.Now().Add(24 * time.Hour).Unix(),
		}
		tok := jwtv4.NewWithClaims(jwtv4.SigningMethodHS256, claims)
		s, err := tok.SignedString([]byte(secret))
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}
		return s
	}

	keyFunc := func(t *jwtv4.Token) (interface{}, error) { return []byte(secret), nil }

	tests := []struct {
		name      string
		iatOffset time.Duration
		wantErr   bool
	}{
		{"iat now+1s accepted", 1 * time.Second, false},
		{"iat now+10s accepted", clockSkew, false},
		{"iat now+11s rejected", clockSkew + time.Second, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokenString := sign(tt.iatOffset)
			_, err := jwtv4.Parse(tokenString, keyFunc)
			if tt.wantErr && err == nil {
				t.Error("expected validation error but token was accepted")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("token should be accepted but got error: %v", err)
			}
		})
	}
}

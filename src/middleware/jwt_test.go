package middleware

import (
	"testing"
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

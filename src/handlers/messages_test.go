package handlers

import (
	"testing"
)

func TestParseAddr(t *testing.T) {
	tests := []struct {
		addr       string
		wantUser   string
		wantDomain string
	}{
		{"@alice@example.com", "alice", "example.com"},
		{"@bob@sub.domain.org", "bob", "sub.domain.org"},
		{"@user@with@extra@domain.com", "user@with@extra", "domain.com"},
		{"@x@y", "x", "y"},
		{"ab", "ab", ""},              // too short
		{"@nodomain", "nodomain", ""}, // no second @
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			user, domain := parseAddr(tt.addr)
			if user != tt.wantUser || domain != tt.wantDomain {
				t.Errorf("parseAddr(%q) = (%q, %q), want (%q, %q)", tt.addr, user, domain, tt.wantUser, tt.wantDomain)
			}
		})
	}
}

func TestIsRecipient(t *testing.T) {
	list := []string{"@alice@example.com", "@bob@example.com"}
	if !isRecipient(list, "@ALICE@example.com") {
		t.Error("expected alice to be a recipient")
	}
	if isRecipient(list, "@charlie@example.com") {
		t.Error("expected charlie not to be a recipient")
	}
	if isRecipient(nil, "@bob@example.com") {
		t.Error("expected nil list to return false")
	}
}

func TestIsZip(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"zip header", []byte{0x50, 0x4b, 0x03, 0x04, 0x00}, true},
		{"exact 4 bytes", []byte{0x50, 0x4b, 0x03, 0x04}, true},
		{"not zip", []byte{0x00, 0x00, 0x00, 0x00}, false},
		{"too short", []byte{0x50, 0x4b}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isZip(tt.data); got != tt.want {
				t.Errorf("isZip(%v) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestMimeToExt(t *testing.T) {
	// The exact extension returned for well-known types depends on the OS MIME
	// database, so we test the fallback paths and the error path.
	tests := []struct {
		mime string
		want string
	}{
		{"totally/invalid;;;", ".bin"},
		{"application/x-unknown-type-fmsg-test", ".bin"},
	}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			got := mimeToExt(tt.mime)
			if got != tt.want {
				t.Errorf("mimeToExt(%q) = %q, want %q", tt.mime, got, tt.want)
			}
		})
	}

	// For well-known types, just verify an extension is returned.
	for _, m := range []string{"text/plain", "text/html", "application/json"} {
		got := mimeToExt(m)
		if got == "" || got[0] != '.' {
			t.Errorf("mimeToExt(%q) = %q, expected a file extension", m, got)
		}
	}
}

func TestCheckDistinctRecipients(t *testing.T) {
	// All distinct.
	if err := checkDistinctRecipients([]string{"@a@b.com", "@c@d.com"}, nil); err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// Duplicate in to (case-insensitive).
	if err := checkDistinctRecipients([]string{"@Alice@B.com", "@alice@b.com"}, nil); err == nil {
		t.Error("expected duplicate error for case-insensitive match in to")
	}

	// Same address in both to and addTo is allowed.
	if err := checkDistinctRecipients([]string{"@a@b.com"}, []string{"@A@B.COM"}); err != nil {
		t.Errorf("expected no error for address in both to and addTo, got %v", err)
	}

	// Duplicate within addTo only.
	if err := checkDistinctRecipients(nil, []string{"@x@y.com", "@X@Y.COM"}); err == nil {
		t.Error("expected duplicate error within addTo")
	}

	// Empty lists.
	if err := checkDistinctRecipients(nil, nil); err != nil {
		t.Errorf("expected no error for empty lists, got %v", err)
	}
}

func TestMsgDataDir(t *testing.T) {
	got := msgDataDir("/data", "@alice@example.com", 42)
	// filepath.Join uses OS separator; check components are present.
	if got == "" {
		t.Fatal("expected non-empty path")
	}
	// Should contain domain/user/out/id segments.
	for _, seg := range []string{"example.com", "alice", "out", "42"} {
		if !contains(got, seg) {
			t.Errorf("msgDataDir result %q missing segment %q", got, seg)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchSubstring(s, sub)
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

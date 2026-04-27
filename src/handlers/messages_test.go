package handlers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestIsTextMIME(t *testing.T) {
	tests := []struct {
		mime string
		want bool
	}{
		{"text/plain", true},
		{"text/html", true},
		{"text/plain; charset=utf-8", true},
		{"TEXT/PLAIN", true},
		{"application/json", false},
		{"application/octet-stream", false},
		{"image/png", false},
		{"application/pdf", false},
		{"", false},
		{"totally invalid;;;", false},
	}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			if got := isTextMIME(tt.mime); got != tt.want {
				t.Errorf("isTextMIME(%q) = %v, want %v", tt.mime, got, tt.want)
			}
		})
	}
}

func TestSafeDataPath(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "sub", "file.txt")
	if _, ok := safeDataPath(inside, dir); !ok {
		t.Errorf("expected inside path to be allowed: %s", inside)
	}
	outside := filepath.Join(filepath.Dir(dir), "evil.txt")
	if _, ok := safeDataPath(outside, dir); ok {
		t.Errorf("expected outside path to be rejected: %s", outside)
	}
	if _, ok := safeDataPath("", dir); ok {
		t.Error("expected empty path to be rejected")
	}
	if _, ok := safeDataPath(inside, ""); ok {
		t.Error("expected empty data dir to be rejected")
	}
}

// writeTempFile writes contents to a fresh file inside dir and returns its path.
func writeTempFile(t *testing.T, dir, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestExtractShortText(t *testing.T) {
	dir := t.TempDir()
	h := &MessageHandler{DataDir: dir, ShortTextSize: 768}

	t.Run("short text/plain returns full content", func(t *testing.T) {
		path := writeTempFile(t, dir, "short.txt", []byte("hello world"))
		got := h.extractShortText(path, "text/plain")
		if got != "hello world" {
			t.Errorf("got %q, want %q", got, "hello world")
		}
	})

	t.Run("ascii longer than max truncates exactly", func(t *testing.T) {
		body := strings.Repeat("a", 2000)
		path := writeTempFile(t, dir, "long.txt", []byte(body))
		got := h.extractShortText(path, "text/plain")
		if len(got) != 768 {
			t.Errorf("got len %d, want 768", len(got))
		}
		if got != strings.Repeat("a", 768) {
			t.Errorf("unexpected content")
		}
	})

	t.Run("utf8 multibyte truncation respects rune boundaries", func(t *testing.T) {
		// "€" is 3 bytes in UTF-8 (0xE2 0x82 0xAC). 768 / 3 = 256 runes
		// (= 768 bytes) — exact boundary. Use 257 runes (771 bytes) so the
		// truncation must drop a partial rune.
		body := strings.Repeat("€", 257)
		path := writeTempFile(t, dir, "utf8.txt", []byte(body))
		small := &MessageHandler{DataDir: dir, ShortTextSize: 770}
		got := small.extractShortText(path, "text/plain; charset=utf-8")
		if !utf8.ValidString(got) {
			t.Errorf("result is not valid UTF-8: % x", got)
		}
		// 770 bytes / 3 bytes per rune => 256 complete runes (768 bytes);
		// trailing 2 bytes of the 257th rune must be dropped.
		if got != strings.Repeat("€", 256) {
			t.Errorf("unexpected truncation: len=%d runes=%d", len(got), utf8.RuneCountInString(got))
		}
	})

	t.Run("non-text mime returns empty", func(t *testing.T) {
		path := writeTempFile(t, dir, "img.bin", []byte("hello"))
		for _, mt := range []string{"application/octet-stream", "image/png", "application/pdf", "application/json"} {
			if got := h.extractShortText(path, mt); got != "" {
				t.Errorf("mime %q: got %q, want empty", mt, got)
			}
		}
	})

	t.Run("invalid utf8 returns empty", func(t *testing.T) {
		path := writeTempFile(t, dir, "bad.txt", []byte{0xff, 0xfe, 0xfd, 0xfc})
		if got := h.extractShortText(path, "text/plain"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("missing file returns empty", func(t *testing.T) {
		got := h.extractShortText(filepath.Join(dir, "does-not-exist.txt"), "text/plain")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("path traversal returns empty", func(t *testing.T) {
		outside := filepath.Join(filepath.Dir(dir), "evil.txt")
		_ = os.WriteFile(outside, []byte("evil"), 0o600)
		defer os.Remove(outside)
		if got := h.extractShortText(outside, "text/plain"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("zero ShortTextSize disables", func(t *testing.T) {
		path := writeTempFile(t, dir, "z.txt", []byte("hello"))
		zero := &MessageHandler{DataDir: dir, ShortTextSize: 0}
		if got := zero.extractShortText(path, "text/plain"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

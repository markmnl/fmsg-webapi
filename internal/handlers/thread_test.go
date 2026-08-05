package handlers

import (
	"strings"
	"testing"
)

func TestRenderThreadText(t *testing.T) {
	ts := float64(1754350000)
	entries := []threadEntry{
		{ID: 1, From: "@bob@example.com", TimeSent: &ts, Type: "text/markdown", Body: "root body\n", Readable: true, Text: true},
		{ID: 2, From: "@alice@hairpin.local", TimeSent: &ts, Type: "image/png", Size: 123, Readable: true},
		{ID: 3, From: "@bob@example.com", TimeSent: &ts, Type: "text/plain", Readable: false},
		{ID: 4, From: "@alice@hairpin.local", TimeSent: &ts, Type: "text/plain", Body: "latest reply", Readable: true, Text: true},
	}
	got := renderThreadText(entries)

	for _, want := range []string{
		"--- @bob@example.com 2025-08-04T23:26:40Z ---\nroot body",
		"[non-text message: image/png, 123 bytes]",
		"[message not visible to you]",
		"latest reply",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "root body") > strings.Index(got, "latest reply") {
		t.Fatal("root must come first (top to bottom)")
	}
}

func TestIsTextType(t *testing.T) {
	for typ, want := range map[string]bool{
		"text/plain": true, "text/markdown": true, "TEXT/HTML": true,
		"application/json": true, "image/png": false, "application/pdf": false, "": false,
	} {
		if isTextType(typ) != want {
			t.Errorf("isTextType(%q) != %v", typ, want)
		}
	}
}

package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		"application/json": true, "application/problem+json": true,
		"text/plain; charset=utf-8": true, "image/png": false, "application/pdf": false, "": false,
	} {
		if isTextType(typ) != want {
			t.Errorf("isTextType(%q) != %v", typ, want)
		}
	}
}

func TestPopulateThreadBodiesAndCacheKeys(t *testing.T) {
	dir := t.TempDir()
	textPath := filepath.Join(dir, "text")
	if err := os.WriteFile(textPath, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	messages := []threadMessage{
		{ID: 1, Visible: true, Type: "text/plain", Size: 5, MessageSHA256: "abc", dataPath: textPath},
		{ID: 2, Visible: true, Type: "application/pdf", Size: 9, dataPath: filepath.Join(dir, "pdf")},
		{ID: 3, Visible: false, Type: "text/plain", Size: 999, dataPath: "/outside"},
	}
	if err := populateThreadBodies(messages, dir, 5); err != nil {
		t.Fatal(err)
	}
	if messages[0].Body == nil || messages[0].Body.Text == nil || *messages[0].Body.Text != "hello" {
		t.Fatalf("text body not inlined: %#v", messages[0].Body)
	}
	if messages[0].Body.CacheKey != "sha256:abc:body" || !messages[0].Body.Cacheable {
		t.Fatalf("unexpected body cache metadata: %#v", messages[0].Body)
	}
	if messages[1].Body.Download != "/fmsg/2/data" || messages[1].Body.Cacheable {
		t.Fatalf("unexpected binary descriptor: %#v", messages[1].Body)
	}
	if messages[2].Body != nil {
		t.Fatal("invisible body must not be populated")
	}
}

func TestThreadMessagesJSONDoesNotExposeInternalPath(t *testing.T) {
	r := threadMessagesResponse{RootID: 1, TriggerID: 1, Complete: true, Messages: []threadMessage{{ID: 1, Visible: true, dataPath: "/secret/path"}}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatalf("internal path leaked: %s", b)
	}
}

func TestThreadAttachmentCacheKey(t *testing.T) {
	if got := partCacheKey("deadbeef", "attachment", 4, "report final.pdf"); got != "sha256:deadbeef:attachment:4:report%20final.pdf" {
		t.Fatalf("got %q", got)
	}
	if got := partCacheKey("", "attachment", 4, "report.pdf"); got != "" {
		t.Fatalf("hashless message must not be cacheable: %q", got)
	}
}

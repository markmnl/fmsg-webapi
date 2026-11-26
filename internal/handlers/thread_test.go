package handlers

import (
	"encoding/json"
	"errors"
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
	if strings.Count(got, "@bob@example.com") != 1 {
		t.Fatalf("a message the caller cannot read must not name its sender:\n%s", got)
	}
	if !strings.Contains(got, "--- hidden ---\n[message not visible to you]") {
		t.Fatalf("missing hidden placeholder in:\n%s", got)
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

func TestPopulateThreadBodies_EmptyDataPathAndSizeLimit(t *testing.T) {
	dir := t.TempDir()
	messages := []threadMessage{
		{ID: 1, Visible: true, Type: "text/plain", Size: 0, MessageSHA256: "abc", dataPath: ""},
	}
	if err := populateThreadBodies(messages, dir, 10); err != nil {
		t.Fatalf("empty dataPath failed: %v", err)
	}
	if messages[0].Body == nil || messages[0].Body.Text == nil || *messages[0].Body.Text != "" || messages[0].Body.Download != "" {
		t.Fatalf("expected empty inlined text body, got %#v", messages[0].Body)
	}

	bigPath := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(bigPath, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	over := []threadMessage{
		{ID: 2, Visible: true, Type: "text/plain", Size: 10, dataPath: bigPath},
	}
	err := populateThreadBodies(over, dir, 5)
	if !errors.Is(err, errThreadTooLarge) {
		t.Fatalf("expected errThreadTooLarge, got %v", err)
	}
}

func TestPopulateThreadBodiesWithEdits_SkipsUneditedDiskRead(t *testing.T) {
	dir := t.TempDir()
	messages := []threadMessage{
		{ID: 1, Visible: true, Type: "text/plain", Size: 5, dataPath: filepath.Join(dir, "missing-original.txt")},
	}
	edits := map[int64]*effectiveEdit{
		1: {msgID: 99, time: 123, hash: []byte{0xde, 0xad}, applied: true, data: []byte("edited")},
	}
	if err := populateThreadBodiesWithEdits(messages, dir, 10, edits); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if messages[0].Body == nil || messages[0].Body.Text == nil || *messages[0].Body.Text != "edited" {
		t.Fatalf("expected edited text body, got %#v", messages[0].Body)
	}
	if messages[0].Body.CacheKey != "sha256:dead:body" {
		t.Fatalf("unexpected cache key: %q", messages[0].Body.CacheKey)
	}

	// Edited body exceeding maxTextBytes must return errThreadTooLarge.
	if err := populateThreadBodiesWithEdits(messages, dir, 3, edits); !errors.Is(err, errThreadTooLarge) {
		t.Fatalf("expected errThreadTooLarge for edited body, got %v", err)
	}
}

func TestPopulateThreadTextEntries_SizeLimitAndEmptyBody(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0600); err != nil {
		t.Fatal(err)
	}

	entries := []threadEntry{
		{ID: 1, Type: "text/plain", Size: 0, Readable: true},
		{ID: 2, Type: "text/plain", Size: 11, Readable: true},
	}
	paths := map[int64]string{1: "", 2: p}
	if err := populateThreadTextEntries(entries, paths, nil, dir, 20); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !entries[0].Text || entries[0].Body != "" {
		t.Fatalf("empty dataPath entry = %+v, want Text=true Body=\"\"", entries[0])
	}
	if !entries[1].Text || entries[1].Body != "hello world" {
		t.Fatalf("file entry = %+v, want Text=true Body=\"hello world\"", entries[1])
	}

	// Exceeding maxTextBytes returns errThreadTooLarge.
	entries2 := []threadEntry{
		{ID: 2, Type: "text/plain", Size: 11, Readable: true},
	}
	if err := populateThreadTextEntries(entries2, paths, nil, dir, 10); !errors.Is(err, errThreadTooLarge) {
		t.Fatalf("expected errThreadTooLarge, got %v", err)
	}

	// Edited entry exceeding maxTextBytes also returns errThreadTooLarge.
	entries3 := []threadEntry{
		{ID: 1, Type: "text/plain", Size: 0, Readable: true},
	}
	edits := map[int64]*effectiveEdit{
		1: {msgID: 50, applied: true, data: []byte("0123456789")},
	}
	if err := populateThreadTextEntries(entries3, paths, edits, dir, 5); !errors.Is(err, errThreadTooLarge) {
		t.Fatalf("expected errThreadTooLarge for edited thread text, got %v", err)
	}
}

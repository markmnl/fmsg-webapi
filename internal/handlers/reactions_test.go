package handlers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/markmnl/fmsg-webapi/internal/models"
)

func TestIsReactionMediaType(t *testing.T) {
	for _, s := range []string{"text/plain;charset=UTF-8", "text/plain; charset=utf-8", "TEXT/PLAIN;CHARSET=UTF-8"} {
		if !isReactionMediaType(s) {
			t.Errorf("isReactionMediaType(%q) = false", s)
		}
	}
	for _, s := range []string{"text/plain", "text/markdown;charset=UTF-8", "application/octet-stream", ""} {
		if isReactionMediaType(s) {
			t.Errorf("isReactionMediaType(%q) = true", s)
		}
	}
}

func TestReactionShape(t *testing.T) {
	ok := func(hasPid, noReply, terminal, important, hasAddTo bool, typ string, size, att int) bool {
		return reactionShape(hasPid, noReply, terminal, important, hasAddTo, typ, size, att)
	}
	if !ok(true, true, true, false, false, reactionMediaType, 4, 0) {
		t.Error("canonical reaction shape rejected")
	}
	if !ok(true, true, true, false, false, reactionMediaType, 0, 0) {
		t.Error("clearing reaction shape rejected")
	}
	cases := map[string]bool{
		"no pid":        ok(false, true, true, false, false, reactionMediaType, 4, 0),
		"no no-reply":   ok(true, false, true, false, false, reactionMediaType, 4, 0),
		"not terminal":  ok(true, true, false, false, false, reactionMediaType, 4, 0),
		"important":     ok(true, true, true, true, false, reactionMediaType, 4, 0),
		"add-to":        ok(true, true, true, false, true, reactionMediaType, 4, 0),
		"wrong type":    ok(true, true, true, false, false, "text/markdown", 4, 0),
		"too big":       ok(true, true, true, false, false, reactionMediaType, maxReactionBytes+1, 0),
		"attachment":    ok(true, true, true, false, false, reactionMediaType, 4, 1),
		"negative size": ok(true, true, true, false, false, reactionMediaType, -1, 0),
	}
	for name, got := range cases {
		if got {
			t.Errorf("%s: shape accepted", name)
		}
	}
}

func TestReactionOfReadsData(t *testing.T) {
	dir := t.TempDir()
	h := &MessageHandler{DataDir: dir}
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	thumbs := write("thumbs", "👍")
	text := write("text", "ok 👍")
	empty := write("empty", "")

	if r := h.reactionOf(true, true, true, false, false, reactionMediaType, 4, 0, thumbs); r == nil || *r != "👍" {
		t.Errorf("reactionOf(thumbs) = %v, want 👍", r)
	}
	if r := h.reactionOf(true, true, true, false, false, reactionMediaType, 6, 0, text); r != nil {
		t.Errorf("reactionOf(text) = %q, want nil", *r)
	}
	if r := h.reactionOf(true, true, true, false, false, reactionMediaType, 0, 0, empty); r == nil || *r != "" {
		t.Errorf("reactionOf(empty) = %v, want \"\"", r)
	}
	if r := h.reactionOf(true, true, true, false, false, reactionMediaType, 4, 0, filepath.Join(dir, "missing")); r != nil {
		t.Errorf("reactionOf(missing file) = %q, want nil", *r)
	}
	if r := h.reactionOf(true, true, true, false, false, reactionMediaType, 4, 0, "/etc/passwd"); r != nil {
		t.Errorf("reactionOf(outside data dir) = %q, want nil", *r)
	}
	if r := h.reactionOf(true, false, true, false, false, reactionMediaType, 4, 0, thumbs); r != nil {
		t.Errorf("reactionOf(wrong shape) = %q, want nil", *r)
	}
}

func TestEffectiveReactions(t *testing.T) {
	rows := []reactionRow{
		{subjectID: 1, msgID: 10, from: "@bob@example.com", time: 100, emoji: "👍"},
		{subjectID: 1, msgID: 11, from: "@carol@example.edu", time: 101, emoji: "👍"},
		{subjectID: 1, msgID: 12, from: "@Bob@example.com", time: 102, emoji: "❤️"}, // bob changes, case-insensitive
		{subjectID: 1, msgID: 13, from: "@dave@example.com", time: 103, emoji: "👍"},
		{subjectID: 1, msgID: 14, from: "@dave@example.com", time: 104, emoji: ""}, // dave clears
		{subjectID: 2, msgID: 20, from: "@bob@example.com", time: 100, emoji: "🎉"},
		{subjectID: 3, msgID: 30, from: "@bob@example.com", time: 100, emoji: ""}, // only a clear
	}
	got := effectiveReactions(rows)
	want := map[int64][]models.Reaction{
		1: {
			{Emoji: "👍", From: []string{"@carol@example.edu"}},
			{Emoji: "❤️", From: []string{"@Bob@example.com"}},
		},
		2: {{Emoji: "🎉", From: []string{"@bob@example.com"}}},
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Errorf("effectiveReactions = %s, want %s", gj, wj)
	}
}

func TestEffectiveReactionsTieBreaksOnHash(t *testing.T) {
	rows := []reactionRow{
		{subjectID: 1, msgID: 10, from: "@bob@example.com", time: 100, hash: []byte{0x01}, emoji: "👍"},
		{subjectID: 1, msgID: 11, from: "@bob@example.com", time: 100, hash: []byte{0x02}, emoji: "❤️"},
	}
	got := effectiveReactions(rows)
	if len(got[1]) != 1 || got[1][0].Emoji != "❤️" {
		t.Errorf("tie at equal time should pick the greater hash, got %+v", got[1])
	}
	// Order independence.
	got = effectiveReactions([]reactionRow{rows[1], rows[0]})
	if len(got[1]) != 1 || got[1][0].Emoji != "❤️" {
		t.Errorf("tie-break must not depend on row order, got %+v", got[1])
	}
}

func TestReactionRecipients(t *testing.T) {
	subject := &models.Message{
		From: "@alice@example.com",
		To:   []string{"@bob@example.com", "@carol@example.edu"},
		AddTo: []models.AddToBatch{
			{AddToFrom: "@bob@example.com", To: []string{"@dave@example.org", "@Carol@example.edu"}},
		},
	}
	got := reactionRecipients(subject, "@BOB@example.com")
	want := []string{"@alice@example.com", "@carol@example.edu", "@dave@example.org"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reactionRecipients = %v, want %v", got, want)
	}
	if got := reactionRecipients(&models.Message{From: "@alice@example.com", To: []string{"@alice@example.com"}}, "@alice@example.com"); len(got) != 0 {
		t.Errorf("self-only message should have no recipients, got %v", got)
	}
}

// A reaction arriving as new_msg is routed to its subject as a reaction event.
func TestHubDispatch_ReactionRoutesToSubject(t *testing.T) {
	var built []int64
	hub := &Hub{
		buildItem: func(_ context.Context, msgID int64, _ string) (*messageListItem, error) {
			built = append(built, msgID)
			return &messageListItem{ID: msgID}, nil
		},
		reactionSubject: func(_ context.Context, msgID int64) (int64, bool) {
			if msgID == 42 {
				return 7, true
			}
			return 0, false
		},
		registry: make(map[string]map[*wsClient]struct{}),
	}
	c := &wsClient{addr: "@bob@example.com", send: make(chan []byte, 4)}
	hub.Register(c)

	subject, ok := hub.reactionSubject(context.Background(), 42)
	if !ok || subject != 7 {
		t.Fatalf("reactionSubject(42) = %d,%v", subject, ok)
	}
	hub.dispatch(context.Background(), subject, "@bob@example.com", eventReaction)
	select {
	case frame := <-c.send:
		var env struct {
			Type string          `json:"type"`
			Data messageListItem `json:"data"`
		}
		if err := json.Unmarshal(frame, &env); err != nil {
			t.Fatal(err)
		}
		if env.Type != eventReaction || env.Data.ID != 7 {
			t.Errorf("got %s for %d, want %s for 7", env.Type, env.Data.ID, eventReaction)
		}
	default:
		t.Fatal("no frame pushed")
	}
	if len(built) != 1 || built[0] != 7 {
		t.Errorf("built %v, want [7] (the subject, not the reaction)", built)
	}
}

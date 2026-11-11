package handlers

import (
	"reflect"
	"testing"

	"github.com/markmnl/fmsg-webapi/internal/edit"
	"github.com/markmnl/fmsg-webapi/internal/models"
)

func TestEditShape(t *testing.T) {
	if !editShape(true, true, true, false, false, edit.MediaType, 0) {
		t.Error("canonical edit shape rejected")
	}
	if !editShape(true, true, true, false, false, "Application/VND.fmsg.Edit", 0) {
		t.Error("type is not matched case-insensitively")
	}
	cases := map[string]bool{
		"no pid":       editShape(false, true, true, false, false, edit.MediaType, 0),
		"no no-reply":  editShape(true, false, true, false, false, edit.MediaType, 0),
		"not terminal": editShape(true, true, false, false, false, edit.MediaType, 0),
		"important":    editShape(true, true, true, true, false, edit.MediaType, 0),
		"add-to":       editShape(true, true, true, false, true, edit.MediaType, 0),
		"wrong type":   editShape(true, true, true, false, false, "application/octet-stream", 0),
		"parameter":    editShape(true, true, true, false, false, edit.MediaType+";v=1", 0),
		"attachment":   editShape(true, true, true, false, false, edit.MediaType, 1),
	}
	for name, got := range cases {
		if got {
			t.Errorf("%s: shape accepted", name)
		}
	}
}

func TestEditTargets(t *testing.T) {
	alice, bob, carol, dave := "@alice@example.com", "@bob@example.com", "@carol@example.com", "@dave@example.com"
	orig, b1, b2 := "aa", "bb", "cc"

	// The original goes to its other participants; each batch to those it added.
	got := editTargets(&models.Message{
		From: alice, To: []string{bob, "@ALICE@example.com", bob}, SHA256: &orig,
		AddTo: []models.AddToBatch{
			{SHA256: &b1, AddToFrom: bob, To: []string{carol, bob}},
			{SHA256: &b2, AddToFrom: alice, To: []string{alice}}, // re-serves only the editor
			{SHA256: &b2, AddToFrom: carol, To: []string{dave}},
		},
	}, alice)
	want := []editTarget{
		{parentHash: &orig, to: []string{bob}},
		{parentHash: &b1, to: []string{carol, bob}},
		{parentHash: &b2, to: []string{dave}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %+v, want %+v", got, want)
	}

	// A message sent only to oneself is edited by a message to oneself.
	self := editTargets(&models.Message{From: alice, To: []string{alice}, SHA256: &orig}, alice)
	if len(self) != 1 || !reflect.DeepEqual(self[0].to, []string{alice}) {
		t.Errorf("note to self targets = %+v", self)
	}
}

func TestShortTextOf(t *testing.T) {
	h := &MessageHandler{ShortTextSize: 5}
	if got := h.shortTextOf([]byte("héllo world"), "text/plain;charset=UTF-8"); got != "héll" {
		t.Errorf("got %q", got)
	}
	if got := h.shortTextOf([]byte("abc"), "application/json"); got != "" {
		t.Errorf("non-text previewed: %q", got)
	}
}

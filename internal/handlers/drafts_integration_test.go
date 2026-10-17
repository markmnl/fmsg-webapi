package handlers

import (
	"encoding/json"
	"testing"
)

// A draft is its sender's alone: its recipients can neither list nor reach it
// by any route until it is sent.
func TestRecipientsCannotSeeDrafts(t *testing.T) {
	a := newFinalizationAPI(t)
	const alice, bob = "@alice@example.com", "@bob@example.com"
	id := a.draft(t, alice, "not ready yet", nil)
	a.upload(t, id)

	listed := func() bool {
		t.Helper()
		w := a.request(bob, "GET", "/fmsg", nil)
		if w.Code != 200 {
			t.Fatalf("list %d: %s", w.Code, w.Body)
		}
		var items []struct {
			ID json.Number `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.ID.String() == id {
				return true
			}
		}
		return false
	}
	if listed() {
		t.Fatal("bob's inbox lists alice's draft")
	}

	for _, r := range []struct{ method, path string }{
		{"GET", "/fmsg/" + id},
		{"GET", "/fmsg/" + id + "/data"},
		{"GET", "/fmsg/" + id + "/attach/note.txt"},
		{"GET", "/fmsg/" + id + "/thread"},
		{"GET", "/fmsg/" + id + "/thread/messages"},
		{"POST", "/fmsg/" + id + "/read"},
	} {
		if w := a.request(bob, r.method, r.path, map[string]any{}); w.Code != 404 {
			t.Errorf("bob %s %s: HTTP %d, want 404: %s", r.method, r.path, w.Code, w.Body)
		}
	}
	if w := a.request(bob, "POST", "/fmsg/"+id+"/react", map[string]any{"emoji": "👍"}); w.Code != 404 {
		t.Errorf("bob react: HTTP %d, want 404: %s", w.Code, w.Body)
	}
	if w := a.request(bob, "POST", "/fmsg/"+id+"/add-to", map[string]any{"add_to": []string{"@carol@example.com"}}); w.Code != 404 {
		t.Errorf("bob add-to: HTTP %d, want 404: %s", w.Code, w.Body)
	}

	// The sender still has it.
	for _, path := range []string{"/fmsg/" + id, "/fmsg/" + id + "/data", "/fmsg/" + id + "/attach/note.txt", "/fmsg/" + id + "/thread/messages"} {
		if w := a.request(alice, "GET", path, nil); w.Code != 200 {
			t.Errorf("alice GET %s: HTTP %d: %s", path, w.Code, w.Body)
		}
	}

	// Once sent, bob has it.
	jsonResponse(t, a.request(alice, "POST", "/fmsg/"+id+"/send", map[string]any{}), 200)
	if !listed() {
		t.Fatal("bob's inbox does not list the sent message")
	}
	for _, path := range []string{"/fmsg/" + id, "/fmsg/" + id + "/data", "/fmsg/" + id + "/attach/note.txt", "/fmsg/" + id + "/thread/messages"} {
		if w := a.request(bob, "GET", path, nil); w.Code != 200 {
			t.Errorf("bob GET %s after send: HTTP %d: %s", path, w.Code, w.Body)
		}
	}
	if w := a.request(bob, "POST", "/fmsg/"+id+"/read", map[string]any{}); w.Code != 200 {
		t.Errorf("bob read after send: HTTP %d: %s", w.Code, w.Body)
	}
}

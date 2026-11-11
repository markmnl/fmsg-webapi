package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/markmnl/fmsg-webapi/internal/edit"
)

const (
	editAlice = "@alice@example.com"
	editBob   = "@bob@example.com"
	editCarol = "@carol@example.com"
)

// sendTo creates and sends a plain-text message from alice, returning its id.
func (a *finalizationAPI) sendTo(t *testing.T, to []string, body string, extra map[string]any) string {
	t.Helper()
	in := map[string]any{"version": 1, "from": editAlice, "to": to, "topic": "edits", "type": "text/plain;charset=UTF-8", "data": body}
	for k, v := range extra {
		in[k] = v
	}
	id := fmt.Sprintf("%.0f", jsonResponse(t, a.request(editAlice, "POST", "/fmsg", in), 201)["id"])
	jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/send", nil), 200)
	return id
}

func (a *finalizationAPI) body(t *testing.T, identity, path string) string {
	t.Helper()
	w := a.request(identity, "GET", path, nil)
	if w.Code != 200 {
		t.Fatalf("GET %s as %s: HTTP %d: %s", path, identity, w.Code, w.Body)
	}
	return w.Body.String()
}

func TestEditLifecycle(t *testing.T) {
	a := newFinalizationAPI(t)
	const original, fixed = "I'll see you their at 5pm", "I'll see you there at 5pm"
	id := a.sendTo(t, []string{editBob}, original, nil)

	// Only the author edits.
	for _, who := range []string{editBob, "@outsider@example.com"} {
		if w := a.request(who, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": "hijacked"}); w.Code != 403 {
			t.Fatalf("%s edit: HTTP %d, want 403: %s", who, w.Code, w.Body)
		}
	}
	// Nothing to do: no edit in force and the data is unchanged, or a revert.
	for _, in := range []map[string]any{{"data": original}, {"data": nil}, {}} {
		if v := jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", in), 200); v["id"] != nil || v["sha256"] != nil {
			t.Fatalf("no-op edit %v returned %v", in, v)
		}
	}

	first := jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": fixed}), 201)
	if len(first["sha256"].(string)) != 64 {
		t.Fatalf("edit response %v", first)
	}
	editID := fmt.Sprintf("%.0f", first["id"])

	// The stored edit message is a compact, well-formed edit document.
	var typ, path string
	var noReply, terminal bool
	if err := a.pool.QueryRow(context.Background(), `SELECT type, filepath, no_reply, is_terminal FROM msg WHERE id=$1`, editID).Scan(&typ, &path, &noReply, &terminal); err != nil {
		t.Fatal(err)
	}
	doc, ok := a.h.readBody(path)
	if !ok || typ != edit.MediaType || !noReply || !terminal {
		t.Fatalf("stored edit: type %q no_reply %v terminal %v read %v", typ, noReply, terminal, ok)
	}
	if got, err := edit.Apply(doc, []byte(original), -1); err != nil || string(got) != fixed {
		t.Fatalf("stored edit document applies to %q, %v", got, err)
	}
	if len(doc) >= 4+5+len(fixed) {
		t.Fatalf("edit document is %d bytes, no smaller than a full replacement", len(doc))
	}

	// Every participant sees the edited data; the original stays available.
	for _, who := range []string{editAlice, editBob} {
		msg := jsonResponse(t, a.request(who, "GET", "/fmsg/"+id, nil), 200)
		edited, _ := msg["edited"].(map[string]any)
		if edited == nil || edited["id"] != first["id"] || edited["size"] != float64(len(fixed)) {
			t.Fatalf("%s: edited = %v", who, msg["edited"])
		}
		if msg["short_text"] != fixed || msg["edit"] != false || msg["size"] != float64(len(original)) {
			t.Fatalf("%s: short_text %v edit %v size %v", who, msg["short_text"], msg["edit"], msg["size"])
		}
		if got := a.body(t, who, "/fmsg/"+id+"/data"); got != fixed {
			t.Fatalf("%s data = %q", who, got)
		}
		if got := a.body(t, who, "/fmsg/"+id+"/data?original=true"); got != original {
			t.Fatalf("%s original data = %q", who, got)
		}
		if got := a.body(t, who, "/fmsg/"+id+"/thread"); !strings.Contains(got, fixed) || strings.Contains(got, original) {
			t.Fatalf("%s thread = %q", who, got)
		}
		thread := a.body(t, who, "/fmsg/"+id+"/thread/messages")
		if !strings.Contains(thread, fixed) || strings.Contains(thread, original) || !strings.Contains(thread, `"edited"`) {
			t.Fatalf("%s thread messages = %s", who, thread)
		}
	}
	if w := a.request("@outsider@example.com", "GET", "/fmsg/"+id+"/data", nil); w.Code != 403 {
		t.Fatalf("outsider data: HTTP %d", w.Code)
	}

	// The edit message itself is flagged so clients hide it, and is listed
	// for its recipient alongside the edited subject.
	if msg := jsonResponse(t, a.request(editBob, "GET", "/fmsg/"+editID, nil), 200); msg["edit"] != true || msg["edited"] != nil {
		t.Fatalf("edit message: edit %v edited %v", msg["edit"], msg["edited"])
	}
	list := a.body(t, editBob, "/fmsg")
	if !strings.Contains(list, `"edit":true`) || !strings.Contains(list, `"short_text":"`+fixed+`"`) {
		t.Fatalf("bob list = %s", list)
	}

	// Repeating the edit sends nothing.
	if again := jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": fixed}), 200); again["id"] != first["id"] {
		t.Fatalf("repeat edit returned %v", again)
	}

	// A second edit is a delta against the original and supersedes the first.
	const second = "See you there at 6pm"
	jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": second}), 201)
	if got := a.body(t, editBob, "/fmsg/"+id+"/data"); got != second {
		t.Fatalf("data after second edit = %q", got)
	}

	// Editing to empty is an edit, not a revert.
	jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": ""}), 201)
	if msg := jsonResponse(t, a.request(editBob, "GET", "/fmsg/"+id, nil), 200); msg["edited"].(map[string]any)["size"] != float64(0) {
		t.Fatalf("edited to empty = %v", msg["edited"])
	}
	if got := a.body(t, editBob, "/fmsg/"+id+"/data"); got != "" {
		t.Fatalf("data after emptying = %q", got)
	}

	// Revert restores the original; reverting again sends nothing.
	revert := jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": nil}), 201)
	msg := jsonResponse(t, a.request(editBob, "GET", "/fmsg/"+id, nil), 200)
	if msg["edited"] != nil || msg["short_text"] != original {
		t.Fatalf("after revert: edited %v short_text %v", msg["edited"], msg["short_text"])
	}
	if got := a.body(t, editBob, "/fmsg/"+id+"/data"); got != original {
		t.Fatalf("data after revert = %q", got)
	}
	if again := jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{}), 200); again["id"] != revert["id"] {
		t.Fatalf("repeat revert returned %v", again)
	}
	// Editing back to the original text after a revert also changes nothing.
	jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": original}), 200)
}

func TestEditRefusals(t *testing.T) {
	a := newFinalizationAPI(t)

	draft := a.draft(t, editAlice, "draft", nil)
	if w := a.request(editAlice, "POST", "/fmsg/"+draft+"/edit", map[string]any{"data": "x"}); w.Code != 409 {
		t.Errorf("edit a draft: HTTP %d, want 409: %s", w.Code, w.Body)
	}
	if w := a.request(editBob, "POST", "/fmsg/"+draft+"/edit", map[string]any{"data": "x"}); w.Code != 404 {
		t.Errorf("edit another's draft: HTTP %d, want 404: %s", w.Code, w.Body)
	}

	terminal := a.sendTo(t, []string{editBob}, "final word", map[string]any{"terminal": true})
	if w := a.request(editAlice, "POST", "/fmsg/"+terminal+"/edit", map[string]any{"data": "x"}); w.Code != 409 {
		t.Errorf("edit a terminal message: HTTP %d, want 409: %s", w.Code, w.Body)
	}

	// An edit message is terminal, so it cannot itself be edited.
	id := a.sendTo(t, []string{editBob}, "hello", nil)
	e := jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": "hello!"}), 201)
	if w := a.request(editAlice, "POST", fmt.Sprintf("/fmsg/%.0f/edit", e["id"]), map[string]any{"data": "x"}); w.Code != 409 {
		t.Errorf("edit an edit: HTTP %d, want 409: %s", w.Code, w.Body)
	}

	if w := a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": strings.Repeat("x", 1<<20+1)}); w.Code != 413 {
		t.Errorf("oversized edit: HTTP %d, want 413", w.Code)
	}
	if w := a.request(editAlice, "POST", "/fmsg/999999/edit", map[string]any{"data": "x"}); w.Code != 404 {
		t.Errorf("edit a missing message: HTTP %d, want 404", w.Code)
	}

	// A no-reply message is still its author's to edit.
	quiet := a.sendTo(t, []string{editBob}, "fyi", map[string]any{"no_reply": true})
	jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+quiet+"/edit", map[string]any{"data": "FYI"}), 201)
}

// An edit-shaped message from someone other than the subject's author, or
// with a malformed edit document, is an ordinary message.
func TestEditIgnoresForgedAndMalformed(t *testing.T) {
	a := newFinalizationAPI(t)
	ctx := context.Background()
	id := a.sendTo(t, []string{editBob}, "the original", nil)
	good := jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": "the edit"}), 201)

	insert := func(from string, doc []byte, when float64) int64 {
		t.Helper()
		var rid int64
		if err := a.pool.QueryRow(ctx,
			`INSERT INTO msg (version, pid, no_reply, is_important, is_deflate, is_terminal, from_addr, topic, type, size, filepath, time_sent)
			 VALUES (1, $1, true, false, false, true, $2, '', $3, $4, '', NULL)
			 RETURNING id`, id, from, edit.MediaType, len(doc),
		).Scan(&rid); err != nil {
			t.Fatal(err)
		}
		path, err := a.h.saveMessageData(from, rid, ".bin", string(doc))
		if err != nil {
			t.Fatal(err)
		}
		// Stamped as sent the way a received message is, without finalizing.
		if _, err := a.pool.Exec(ctx,
			`UPDATE msg SET filepath=$1, time_sent=$2, sha256=sha256($3::bytea), wire_header='\x00', wire_message='{}' WHERE id=$4`,
			path, when, fmt.Sprintf("forged %s %f", from, when), rid); err != nil {
			t.Fatal(err)
		}
		return rid
	}
	later := good["time"].(float64) + 60
	forged := insert(editBob, edit.Diff([]byte("the original"), []byte("bob was here")), later)
	malformed := insert(editAlice, []byte{9, 0, 0, 0, 0, 0xff, 0, 0, 0, 1, 0, 0, 0}, later+1)

	msg := jsonResponse(t, a.request(editBob, "GET", "/fmsg/"+id, nil), 200)
	if msg["edited"].(map[string]any)["id"] != good["id"] {
		t.Fatalf("effective edit = %v, want %v", msg["edited"], good["id"])
	}
	if got := a.body(t, editBob, "/fmsg/"+id+"/data"); got != "the edit" {
		t.Fatalf("data = %q", got)
	}
	for _, rid := range []int64{forged, malformed} {
		if _, ok := a.h.editSubject(ctx, rid); ok {
			t.Errorf("message %d recognised as an edit", rid)
		}
	}
}

func TestEditReachesAddedRecipients(t *testing.T) {
	a := newFinalizationAPI(t)
	id := a.sendTo(t, []string{editBob}, "meet at noon", nil)
	batch := jsonResponse(t, a.request(editBob, "POST", "/fmsg/"+id+"/add-to", map[string]any{"add_to": []string{editCarol}}), 200)
	jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": "meet at one"}), 201)

	// One edit message per branch: the original's to bob, the batch's to carol.
	rows, err := a.pool.Query(context.Background(),
		`SELECT encode(m.psha256,'hex'), (SELECT string_agg(addr, ',') FROM msg_to t WHERE t.msg_id = m.id)
		 FROM msg m WHERE m.pid = $1 AND m.type = $2 ORDER BY m.id`, id, edit.MediaType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var parent, to string
		if err := rows.Scan(&parent, &to); err != nil {
			t.Fatal(err)
		}
		got[parent] = to
	}
	subject := jsonResponse(t, a.request(editAlice, "GET", "/fmsg/"+id, nil), 200)
	want := map[string]string{subject["sha256"].(string): editBob, batch["sha256"].(string): editCarol}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("edit messages = %v, want %v", got, want)
	}

	for _, who := range []string{editBob, editCarol} {
		if got := a.body(t, who, "/fmsg/"+id+"/data"); got != "meet at one" {
			t.Fatalf("%s data = %q", who, got)
		}
	}
}

// A message sent only to oneself can be edited and reacted to: the edit and
// the reaction are addressed to the sender (FMSG-008, FMSG-005 v0.1.1).
func TestEditAndReactNoteToSelf(t *testing.T) {
	a := newFinalizationAPI(t)
	id := a.sendTo(t, []string{editAlice}, "remember teh milk", nil)

	jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/edit", map[string]any{"data": "remember the milk"}), 201)
	if got := a.body(t, editAlice, "/fmsg/"+id+"/data"); got != "remember the milk" {
		t.Fatalf("data = %q", got)
	}

	jsonResponse(t, a.request(editAlice, "POST", "/fmsg/"+id+"/react", map[string]any{"emoji": "👍"}), 201)
	msg := jsonResponse(t, a.request(editAlice, "GET", "/fmsg/"+id, nil), 200)
	if reactions, _ := msg["reactions"].([]any); len(reactions) != 1 {
		t.Fatalf("reactions = %v", msg["reactions"])
	}
}

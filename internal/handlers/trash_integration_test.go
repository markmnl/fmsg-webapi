package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

const (
	alice = "@alice@example.com"
	bob   = "@bob@example.com"
	carol = "@carol@example.com"
)

// usageLog records the usage fmsgid is told about.
type usageLog struct {
	mu    sync.Mutex
	posts []string // "<path> <address> <size>"
}

func (u *usageLog) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var tx struct {
				Address string  `json:"address"`
				TS      float64 `json:"ts"`
				Size    int64   `json:"size"`
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &tx)
			u.mu.Lock()
			u.posts = append(u.posts, fmt.Sprintf("%s %s %d", r.URL.Path, tx.Address, tx.Size))
			u.mu.Unlock()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"acceptingNew":true}`)
	})
}

// releases lists the release posts made so far.
func (u *usageLog) releases() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, p := range u.posts {
		if len(p) > len("/fmsgid/send/release") && (p[:len("/fmsgid/send/release")] == "/fmsgid/send/release" || p[:len("/fmsgid/recv/release")] == "/fmsgid/recv/release") {
			out = append(out, p)
		}
	}
	return out
}

// sendAs sends a new message from `from` to `to` (replying to pid when set)
// and returns its id.
func (a *finalizationAPI) sendAs(t *testing.T, from string, to []string, body string, pid any) string {
	t.Helper()
	v := jsonResponse(t, a.request(from, "POST", "/fmsg", map[string]any{
		"version": 1, "from": from, "to": to, "type": "text/plain;charset=UTF-8", "data": body, "pid": pid,
	}), 201)
	id := fmt.Sprintf("%.0f", v["id"])
	jsonResponse(t, a.request(from, "POST", "/fmsg/"+id+"/send", map[string]any{}), 200)
	return id
}

// listed returns the trash state of each message in identity's inbox for the
// trash mode ("" for none).
func (a *finalizationAPI) listed(t *testing.T, identity, mode string) map[string]any {
	t.Helper()
	path := "/fmsg?limit=100"
	if mode != "" {
		path += "&trash=" + mode
	}
	w := a.request(identity, "GET", path, nil)
	if w.Code != 200 {
		t.Fatalf("list %s: %d %s", mode, w.Code, w.Body)
	}
	var items []struct {
		ID    json.Number `json:"id"`
		Trash any         `json:"trash"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, it := range items {
		out[it.ID.String()] = it.Trash
	}
	return out
}

func wantListed(t *testing.T, got map[string]any, id string, want any) {
	t.Helper()
	state, ok := got[id]
	if want == "absent" {
		if ok {
			t.Errorf("message %s listed (trash %v), want absent", id, state)
		}
		return
	}
	if !ok {
		t.Errorf("message %s not listed, want trash %v", id, want)
		return
	}
	if state != want {
		t.Errorf("message %s trash = %v, want %v", id, state, want)
	}
}

// Deleting moves a message, or a whole thread with its later replies, into the
// deleter's trash only, and restoring brings it back.
func TestTrashAndRestore(t *testing.T) {
	a := newFinalizationAPI(t)
	root := a.sendAs(t, alice, []string{bob, alice}, "root", nil)
	reply := a.sendAs(t, alice, []string{bob, alice}, "reply", mustID(t, root))

	if w := a.request(bob, "GET", "/fmsg?trash=bogus", nil); w.Code != 400 {
		t.Errorf("bad trash mode: HTTP %d, want 400", w.Code)
	}
	if w := a.request(carol, "DELETE", "/fmsg/"+root, nil); w.Code != 404 {
		t.Errorf("non-participant delete: HTTP %d, want 404", w.Code)
	}

	// One message.
	v := jsonResponse(t, a.request(bob, "DELETE", "/fmsg/"+root, nil), 200)
	if v["trash"] != "message" {
		t.Errorf("delete response trash = %v", v["trash"])
	}
	wantListed(t, a.listed(t, bob, ""), root, "absent")
	wantListed(t, a.listed(t, bob, ""), reply, nil)
	wantListed(t, a.listed(t, bob, "include"), root, "message")
	only := a.listed(t, bob, "only")
	wantListed(t, only, root, "message")
	wantListed(t, only, reply, "absent")
	if got := jsonResponse(t, a.request(bob, "GET", "/fmsg/"+root, nil), 200)["trash"]; got != "message" {
		t.Errorf("get trash = %v", got)
	}
	wantListed(t, a.listed(t, alice, ""), root, nil) // alice's inbox is her own

	// The whole thread, including a reply that arrives afterwards.
	jsonResponse(t, a.request(bob, "DELETE", "/fmsg/"+root+"?thread=true", nil), 200)
	later := a.sendAs(t, alice, []string{bob, alice}, "later", mustID(t, reply))
	def := a.listed(t, bob, "")
	for _, id := range []string{root, reply, later} {
		wantListed(t, def, id, "absent")
	}
	inc := a.listed(t, bob, "include")
	wantListed(t, inc, root, "thread")
	wantListed(t, inc, later, "thread")
	item, err := a.h.messageItemFor(context.Background(), mustID(t, later), bob)
	if err != nil || item.Trash == nil || *item.Trash != "thread" {
		t.Fatalf("pushed item trash = %v, %v; a reply in a deleted thread must not be pushed", item, err)
	}
	// It still shows as unread there.
	if item.Read {
		t.Error("later reply should be unread")
	}

	// Restore the thread.
	jsonResponse(t, a.request(bob, "POST", "/fmsg/"+root+"/restore?thread=true", map[string]any{}), 200)
	def = a.listed(t, bob, "")
	for _, id := range []string{root, reply, later} {
		wantListed(t, def, id, nil)
	}

	// Drafts are still deleted outright by their owner.
	draft := a.draft(t, alice, "unsent", nil)
	if w := a.request(alice, "DELETE", "/fmsg/"+draft, nil); w.Code != 204 {
		t.Errorf("draft delete: HTTP %d, want 204", w.Code)
	}
}

// Deleting for good hides a message from the deleter everywhere, gives back
// its usage, and removes it from the host once nobody here holds it.
func TestPurge(t *testing.T) {
	usage := &usageLog{}
	a := newFinalizationAPIWithID(t, usage.handler())
	ctx := context.Background()
	root := a.sendAs(t, alice, []string{bob, alice}, "root", nil)
	reply := a.sendAs(t, bob, []string{alice, bob, carol}, "reply", mustID(t, root))
	var body, hash string
	if err := a.pool.QueryRow(ctx, `SELECT filepath, encode(sha256,'hex') FROM msg WHERE id = $1`, mustID(t, root)).Scan(&body, &hash); err != nil {
		t.Fatal(err)
	}
	size, err := a.h.messageStoredSize(ctx, mustID(t, root))
	if err != nil {
		t.Fatal(err)
	}

	// Bob deletes the root for good: alice still holds it.
	v := jsonResponse(t, a.request(bob, "DELETE", "/fmsg/"+root+"?permanent=true", nil), 200)
	if v["purged"] != float64(2) || v["removed"] != float64(0) { // the root and bob's reply below it
		t.Fatalf("bob purge = %v", v)
	}
	for _, r := range []struct{ method, path string }{
		{"GET", "/fmsg/" + root},
		{"GET", "/fmsg/" + root + "/data"},
		{"GET", "/fmsg/" + root + "/thread"},
		{"GET", "/fmsg/" + root + "/thread/messages"},
		{"POST", "/fmsg/" + root + "/read"},
		{"POST", "/fmsg/" + root + "/restore"},
		{"DELETE", "/fmsg/" + root},
	} {
		if w := a.request(bob, r.method, r.path, map[string]any{}); w.Code != 404 {
			t.Errorf("bob %s %s after purge: HTTP %d, want 404", r.method, r.path, w.Code)
		}
	}
	wantListed(t, a.listed(t, bob, "include"), root, "absent")
	if w := a.request(bob, "POST", "/fmsg", map[string]any{"version": 1, "from": bob, "to": []string{alice}, "type": "text/plain;charset=UTF-8", "data": "re", "pid": mustID(t, root)}); w.Code != 400 {
		t.Errorf("reply to a purged parent: HTTP %d, want 400: %s", w.Code, w.Body)
	}
	if w := a.request(alice, "GET", "/fmsg/"+root, nil); w.Code != 200 {
		t.Errorf("alice get after bob's purge: HTTP %d, want 200", w.Code)
	}
	wantRelease(t, usage, fmt.Sprintf("/fmsgid/recv/release %s %d", bob, size))

	// Alice deletes the root for good: nobody holds it now. Carol still
	// holds the reply, which stays, no longer linked to it.
	v = jsonResponse(t, a.request(alice, "DELETE", "/fmsg/"+root+"?permanent=true", nil), 200)
	if v["removed"] != float64(1) {
		t.Fatalf("alice purge = %v", v)
	}
	var n int
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM msg WHERE encode(sha256,'hex') = $1`, hash).Scan(&n); err != nil || n != 0 {
		t.Fatalf("root still stored: %d, %v", n, err)
	}
	if _, err := os.Stat(body); !os.IsNotExist(err) {
		t.Errorf("root body still on disk: %v", err)
	}
	var pid *int64
	if err := a.pool.QueryRow(ctx, `SELECT pid FROM msg WHERE id = $1`, mustID(t, reply)).Scan(&pid); err != nil || pid != nil {
		t.Fatalf("reply after its parent was removed: pid %v, %v", pid, err)
	}
	if w := a.request(carol, "GET", "/fmsg/"+reply, nil); w.Code != 200 {
		t.Errorf("carol's reply: HTTP %d, want 200", w.Code)
	}
	// Alice sent and received the root: both come back to her.
	wantRelease(t, usage, fmt.Sprintf("/fmsgid/send/release %s %d", alice, size))
	wantRelease(t, usage, fmt.Sprintf("/fmsgid/recv/release %s %d", alice, size))

	// Deleting the root for good took the reply below it with it, once.
	replySize, err := a.h.messageStoredSize(ctx, mustID(t, reply))
	if err != nil {
		t.Fatal(err)
	}
	wantRelease(t, usage, fmt.Sprintf("/fmsgid/recv/release %s %d", alice, replySize))
	before := len(usage.releases())
	if w := a.request(alice, "DELETE", "/fmsg/"+reply+"?permanent=true", nil); w.Code != 404 {
		t.Errorf("purge twice: HTTP %d, want 404", w.Code)
	}
	if w := a.request(alice, "GET", "/fmsg/"+reply, nil); w.Code != 404 {
		t.Errorf("alice get reply below her purged root: HTTP %d, want 404", w.Code)
	}
	if got := len(usage.releases()) - before; got != 0 {
		t.Errorf("purging again released %d more", got)
	}
}

// A delivery fmsgd still retries keeps the message, whatever the code it
// retries after; one that ended with a final code does not.
func TestPurgeKeepsRetryableDelivery(t *testing.T) {
	a := newFinalizationAPI(t)
	ctx := context.Background()
	full := a.sendAs(t, alice, []string{"@dave@remote.example"}, "user full", nil)
	if _, err := a.pool.Exec(ctx, `UPDATE msg_to SET response_code = 101, attempt_count = 1, time_last_attempt = $2 WHERE msg_id = $1`, mustID(t, full), now()); err != nil {
		t.Fatal(err)
	}
	if v := jsonResponse(t, a.request(alice, "DELETE", "/fmsg/"+full+"?permanent=true", nil), 200); v["removed"] != float64(0) {
		t.Errorf("delivery fmsgd retries (user full) removed: %v", v)
	}
	unknown := a.sendAs(t, alice, []string{"@dave@remote.example"}, "user unknown", nil)
	if _, err := a.pool.Exec(ctx, `UPDATE msg_to SET response_code = 100, attempt_count = 1, time_last_attempt = $2 WHERE msg_id = $1`, mustID(t, unknown), now()); err != nil {
		t.Fatal(err)
	}
	if v := jsonResponse(t, a.request(alice, "DELETE", "/fmsg/"+unknown+"?permanent=true", nil), 200); v["removed"] != float64(1) {
		t.Errorf("delivery that ended with a final code kept: %v", v)
	}
}

// A delivery fmsgd has given up on (older than the retry window, still
// undelivered) no longer keeps the message, nor does an add-to participant
// notification it has given up on.
func TestPurgeRemovesGivenUpDelivery(t *testing.T) {
	a := newFinalizationAPI(t)
	ctx := context.Background()
	a.h.RetryMaxAge = 0.2

	// Never answered: fmsgd records -1 and retries until the window closes.
	id := a.sendAs(t, alice, []string{"@dave@remote.example"}, "out", nil)
	if _, err := a.pool.Exec(ctx, `UPDATE msg_to SET response_code = -1, attempt_count = 5, time_last_attempt = $2 WHERE msg_id = $1`, mustID(t, id), now()); err != nil {
		t.Fatal(err)
	}
	// Delivered to dave, then erin added: erin's delivery and the notification
	// owed to dave's domain are pending until they age out.
	added := a.sendAs(t, alice, []string{"@dave@remote.example"}, "added to", nil)
	if _, err := a.pool.Exec(ctx, `UPDATE msg_to SET time_delivered = $2, response_code = 0 WHERE msg_id = $1`, mustID(t, added), now()); err != nil {
		t.Fatal(err)
	}
	jsonResponse(t, a.request(alice, "POST", "/fmsg/"+added+"/add-to", map[string]any{"add_to": []string{"@erin@other.example"}}), 200)
	var notifies int
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM msg_add_to_notify n JOIN msg_add_to_batch b ON b.id = n.batch_id WHERE b.msg_id = $1 AND n.time_notified IS NULL`, mustID(t, added)).Scan(&notifies); err != nil || notifies != 1 {
		t.Fatalf("pending notify rows = %d, %v; want 1", notifies, err)
	}
	for _, q := range []string{
		`UPDATE msg_add_to SET response_code = -1, attempt_count = 5, time_last_attempt = $2 WHERE msg_id = $1`,
		`UPDATE msg_add_to_notify SET response_code = -1, attempt_count = 5, time_last_attempt = $2 WHERE batch_id IN (SELECT id FROM msg_add_to_batch WHERE msg_id = $1)`,
	} {
		if _, err := a.pool.Exec(ctx, q, mustID(t, added), now()); err != nil {
			t.Fatal(err)
		}
	}

	// Inside the window both are kept (only hidden from alice).
	if v := jsonResponse(t, a.request(alice, "DELETE", "/fmsg/"+id+"?permanent=true", nil), 200); v["removed"] != float64(0) {
		t.Fatalf("delivery still in the retry window removed: %v", v)
	}
	time.Sleep(300 * time.Millisecond)

	// Past it, deleting for good removes the message from the host.
	if v := jsonResponse(t, a.request(alice, "DELETE", "/fmsg/"+added+"?permanent=true", nil), 200); v["removed"] != float64(1) {
		t.Errorf("given-up add-to delivery and notification kept the message: %v", v)
	}
	if a.exists(t, added) {
		t.Error("message reported removed is still stored")
	}
	if !a.exists(t, id) {
		t.Error("message deleted for good inside the window was removed without a purge")
	}
}

// exists reports whether message id is still stored on the host.
func (a *finalizationAPI) exists(t *testing.T, id string) bool {
	t.Helper()
	var ok bool
	if err := a.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM msg WHERE id = $1)`, mustID(t, id)).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// now is the current time as fmsgd stores times: seconds since the epoch.
func now() float64 { return float64(time.Now().UnixMicro()) / 1e6 }

// A message still being delivered elsewhere is not removed.
func TestPurgeKeepsPendingDelivery(t *testing.T) {
	a := newFinalizationAPI(t)
	id := a.sendAs(t, alice, []string{"@dave@remote.example"}, "out", nil)
	v := jsonResponse(t, a.request(alice, "DELETE", "/fmsg/"+id+"?permanent=true", nil), 200)
	if v["removed"] != float64(0) {
		t.Fatalf("pending delivery removed: %v", v)
	}
	if w := a.request(alice, "GET", "/fmsg/"+id, nil); w.Code != 404 {
		t.Errorf("sender get after purge: HTTP %d, want 404", w.Code)
	}
}

func wantRelease(t *testing.T, u *usageLog, want string) {
	t.Helper()
	for _, p := range u.releases() {
		if p == want {
			return
		}
	}
	t.Errorf("no release %q in %v", want, u.releases())
}

// Moving a message to the trash notifies msg_trash for the deleter.
func TestTrashNotifies(t *testing.T) {
	a := newFinalizationAPI(t)
	ctx := context.Background()
	id := a.sendAs(t, alice, []string{bob, alice}, "hello", nil)
	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN msg_trash"); err != nil {
		t.Fatal(err)
	}
	jsonResponse(t, a.request(bob, "DELETE", "/fmsg/"+id, nil), 200)
	wait, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(wait)
	if err != nil {
		t.Fatalf("no msg_trash notification: %v", err)
	}
	if want := id + "," + bob; n.Payload != want {
		t.Errorf("payload %q, want %q", n.Payload, want)
	}
}

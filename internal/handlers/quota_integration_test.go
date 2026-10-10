package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// quotaFmsgID serves fmsgid's address detail from per-address overrides
// (unlimited and accepting by default; "@nobody@…" is unknown) and records the
// usage posted to it.
type quotaFmsgID struct {
	mu      sync.Mutex
	details map[string]map[string]any
	down    bool
	usage   []postedUsage
}

type postedUsage struct {
	Kind    string  // "recv" or "send"
	Address string  `json:"address"`
	Ts      float64 `json:"ts"`
	Size    int64   `json:"size"`
}

func newQuotaFmsgID() *quotaFmsgID { return &quotaFmsgID{details: map[string]map[string]any{}} }

func (f *quotaFmsgID) set(addr string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.details[strings.ToLower(addr)] = fields
}

func (f *quotaFmsgID) posted(kind string) []postedUsage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []postedUsage
	for _, u := range f.usage {
		if u.Kind == kind {
			out = append(out, u)
		}
	}
	return out
}

func (f *quotaFmsgID) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/fmsgid/recv" || r.URL.Path == "/fmsgid/send"):
		u := postedUsage{Kind: strings.TrimPrefix(r.URL.Path, "/fmsgid/")}
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.usage = append(f.usage, u)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/fmsgid/"):
		if f.down {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		addr := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/fmsgid/"))
		if strings.HasPrefix(addr, "@nobody@") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		detail := map[string]any{"address": addr, "acceptingNew": true}
		for k, v := range f.details[addr] {
			detail[k] = v
		}
		_ = json.NewEncoder(w).Encode(detail)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// sendDraft creates alice's draft to `to` with body, and an attachment when
// attach is set, returning its id.
func (a *finalizationAPI) sendDraft(t *testing.T, to []string, body string, attach bool) string {
	t.Helper()
	v := jsonResponse(t, a.request("@alice@example.com", "POST", "/fmsg", map[string]any{
		"version": 1, "from": "@alice@example.com", "to": to, "type": "text/plain;charset=UTF-8", "data": body,
	}), 201)
	id := fmt.Sprintf("%.0f", v["id"])
	if attach {
		a.upload(t, id) // "note.txt", 16 bytes
	}
	return id
}

type recipientRow struct {
	delivered bool
	code      *int
}

func (a *finalizationAPI) recipientRows(t *testing.T, table, id string) map[string]recipientRow {
	t.Helper()
	rows, err := a.pool.Query(context.Background(),
		"SELECT addr, time_delivered IS NOT NULL, response_code FROM "+table+" WHERE msg_id = $1", mustID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]recipientRow{}
	for rows.Next() {
		var addr string
		var r recipientRow
		var code *int16
		if err := rows.Scan(&addr, &r.delivered, &code); err != nil {
			t.Fatal(err)
		}
		if code != nil {
			c := int(*code)
			r.code = &c
		}
		out[addr] = r
	}
	return out
}

func wantRow(t *testing.T, rows map[string]recipientRow, addr string, delivered bool, code int) {
	t.Helper()
	r, ok := rows[addr]
	if !ok || r.delivered != delivered || r.code == nil || *r.code != code {
		t.Fatalf("%s: got %+v (code %v), want delivered=%v code=%d", addr, r, r.code, delivered, code)
	}
}

func TestLocalDeliveryChecksAndRecordsReceiveQuota(t *testing.T) {
	id := newQuotaFmsgID()
	a := newFinalizationAPIWithID(t, id)
	bob, carol, dave := "@bob@example.com", "@carol@example.com", "@dave@example.com"
	id.set(carol, map[string]any{"limitRecvCountPer1d": 3, "recvCountPer1d": 3})
	// dave's storage has room for the body but not the attachment
	id.set(dave, map[string]any{"limitRecvSizeTotal": 100, "recvSizeTotal": 80})

	start := float64(time.Now().Unix())
	msg := a.sendDraft(t, []string{bob, carol, dave, "@nobody@example.com", "@remote@other.example"}, "hello", true)
	const size = 5 + 16 // body + attachment
	jsonResponse(t, a.request("@alice@example.com", "POST", "/fmsg/"+msg+"/send", nil), 200)

	rows := a.recipientRows(t, "msg_to", msg)
	wantRow(t, rows, bob, true, responseCodeAccept)
	wantRow(t, rows, carol, false, responseCodeUserFull)
	wantRow(t, rows, dave, false, responseCodeUserFull)
	wantRow(t, rows, "@nobody@example.com", false, responseCodeUserUnknown)
	if r := rows["@remote@other.example"]; r.delivered || r.code != nil {
		t.Fatalf("remote recipient resolved locally: %+v", r)
	}

	// The sender sees each local recipient's outcome.
	got := jsonResponse(t, a.request("@alice@example.com", "GET", "/fmsg/"+msg, nil), 200)
	codes := map[string]any{}
	for _, d := range got["to_delivery"].([]any) {
		d := d.(map[string]any)
		codes[d["addr"].(string)] = d["response_code"]
	}
	if codes[carol] != float64(101) || codes[bob] != float64(200) {
		t.Fatalf("to_delivery codes: %v", codes)
	}

	recv := id.posted("recv")
	if len(recv) != 1 || recv[0].Address != bob || recv[0].Size != size || recv[0].Ts < start {
		t.Fatalf("recv usage: %+v, want one for %s of %d bytes", recv, bob, size)
	}
	send := id.posted("send")
	if len(send) != 1 || send[0].Address != "@alice@example.com" || send[0].Size != size || send[0].Ts < start {
		t.Fatalf("send usage: %+v, want one for alice of %d bytes", send, size)
	}

	// add-to: the new local recipients' receive limits are checked and their
	// usage recorded; adding recipients sends no new message.
	erin, frank := "@erin@example.com", "@frank@example.com"
	id.set(frank, map[string]any{"limitRecvSizePer1d": 10})
	jsonResponse(t, a.request("@alice@example.com", "POST", "/fmsg/"+msg+"/add-to", map[string]any{"add_to": []string{erin, frank}}), 200)
	rows = a.recipientRows(t, "msg_add_to", msg)
	wantRow(t, rows, erin, true, responseCodeAccept)
	wantRow(t, rows, frank, false, responseCodeUserFull)
	if recv := id.posted("recv"); len(recv) != 2 || recv[1].Address != erin || recv[1].Size != size {
		t.Fatalf("recv usage after add-to: %+v", recv)
	}
	if send := id.posted("send"); len(send) != 1 {
		t.Fatalf("add-to recorded send usage: %+v", send)
	}

	// A reaction is a message: sender and local recipients are counted.
	jsonResponse(t, a.request(bob, "POST", "/fmsg/"+msg+"/react", map[string]any{"emoji": "👍"}), 201)
	send = id.posted("send")
	if len(send) != 2 || send[1].Address != bob || send[1].Size != int64(len("👍")) {
		t.Fatalf("reaction send usage: %+v", send)
	}
	// carol is still full, and nobody unknown; alice, dave, erin and frank
	// each have room for the reaction.
	recv = id.posted("recv")[2:]
	gotRecv := map[string]int64{}
	for _, u := range recv {
		gotRecv[u.Address] = u.Size
	}
	want := map[string]int64{"@alice@example.com": 4, dave: 4, erin: 4, frank: 4}
	if fmt.Sprint(gotRecv) != fmt.Sprint(want) || len(recv) != len(want) {
		t.Fatalf("reaction recv usage: %+v, want %v", recv, want)
	}
}

func TestSendRefusedOverSendQuota(t *testing.T) {
	id := newQuotaFmsgID()
	a := newFinalizationAPIWithID(t, id)
	alice, bob := "@alice@example.com", "@bob@example.com"
	tests := []struct {
		name   string
		limits map[string]any
		status int
		code   string
		error  string
	}{
		{"daily count", map[string]any{"limitSendCountPer1d": 10, "sendCountPer1d": 10}, http.StatusTooManyRequests, "send_count_per_1d_limit", "daily message limit reached"},
		{"daily size", map[string]any{"limitSendSizePer1d": 30, "sendSizePer1d": 10}, http.StatusTooManyRequests, "send_size_per_1d_limit", "daily sending size limit reached"},
		{"storage", map[string]any{"limitSendSizeTotal": 1000, "sendSizeTotal": 990}, http.StatusUnprocessableEntity, "send_size_total_limit", "storage limit reached"},
		{"per message, attachments included", map[string]any{"limitSendSizePerMsg": 20}, http.StatusRequestEntityTooLarge, "send_size_per_msg_limit", "exceeds the sending limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id.set(alice, tt.limits)
			msg := a.sendDraft(t, []string{bob}, "hello", true) // 21 bytes
			w := a.request(alice, "POST", "/fmsg/"+msg+"/send", nil)
			v := jsonResponse(t, w, tt.status)
			if v["code"] != tt.code || !strings.Contains(v["error"].(string), tt.error) {
				t.Fatalf("refusal: %v", v)
			}
			draft := jsonResponse(t, a.request(alice, "GET", "/fmsg/"+msg, nil), 200)
			if draft["time"] != nil {
				t.Fatal("refused message was sent")
			}
			if rows := a.recipientRows(t, "msg_to", msg); rows[bob].delivered || rows[bob].code != nil {
				t.Fatalf("refused message was delivered: %+v", rows[bob])
			}
		})
	}
	if send, recv := id.posted("send"), id.posted("recv"); len(send) != 0 || len(recv) != 0 {
		t.Fatalf("refused sends recorded usage: send=%v recv=%v", send, recv)
	}

	// Within the limits, the same message sends.
	id.set(alice, map[string]any{"limitSendCountPer1d": 10, "sendCountPer1d": 9, "limitSendSizePerMsg": 21})
	msg := a.sendDraft(t, []string{bob}, "hello", true)
	jsonResponse(t, a.request(alice, "POST", "/fmsg/"+msg+"/send", nil), 200)

	// A reaction over the reactor's daily count is refused and not sent.
	id.set(bob, map[string]any{"limitSendCountPer1d": 0})
	if w := a.request(bob, "POST", "/fmsg/"+msg+"/react", map[string]any{"emoji": "👍"}); w.Code != http.StatusTooManyRequests {
		t.Fatalf("reaction over quota: %d %s", w.Code, w.Body)
	}
	var reactions int
	if err := a.pool.QueryRow(context.Background(), "SELECT count(*) FROM msg WHERE pid = $1", mustID(t, msg)).Scan(&reactions); err != nil || reactions != 0 {
		t.Fatalf("refused reaction stored: %d %v", reactions, err)
	}

	// fmsgid unavailable: refuse rather than send unchecked.
	id.mu.Lock()
	id.down = true
	id.mu.Unlock()
	msg = a.sendDraft(t, []string{bob}, "hello", false)
	if w := a.request(alice, "POST", "/fmsg/"+msg+"/send", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("send with fmsgid down: %d %s", w.Code, w.Body)
	}
}
